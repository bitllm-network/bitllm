package node

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	p2pproto "github.com/libp2p/go-libp2p/core/protocol"
)

// setVar changes a package var for one test. Nodes read these once, in New, so
// set them before starting the node.
func setVar[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// scriptedPeer is a bare libp2p host connected to server, which speaks the
// infer protocol by hand and is free to misbehave. A nil key makes a new id.
func scriptedPeer(t *testing.T, server *Node, key crypto.PrivKey, addr string) host.Host {
	t.Helper()
	opts := []libp2p.Option{libp2p.NoListenAddrs, libp2p.DisableRelay()}
	if key != nil {
		opts = append(opts, libp2p.Identity(key))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if addr == "" {
		addr = server.Addrs()[0]
	}
	ai, err := peer.AddrInfoFromString(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Connect(t.Context(), *ai); err != nil {
		t.Fatal(err)
	}
	return h
}

// openInfer opens an infer stream and writes one request line.
func openInfer(t *testing.T, h host.Host, server *Node, proto p2pproto.ID, req inferReq, closeWrite bool) network.Stream {
	t.Helper()
	s, err := h.NewStream(t.Context(), server.host.ID(), proto)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Reset() })
	b, _ := json.Marshal(req)
	if _, err := s.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	if closeWrite {
		s.CloseWrite()
	}
	return s
}

// chatReq is a small chat request for model ds that may wait in the queue.
func chatReq(wait time.Duration) inferReq {
	return inferReq{Path: "/chat/completions", Wait: wait.Milliseconds(),
		Body: json.RawMessage(`{"model":"ds","messages":[{"role":"user","content":"hi"}]}`)}
}

// readHdr reads one header line and fails the test if none comes in time.
func readHdr(t *testing.T, br *bufio.Reader, s network.Stream, within time.Duration) (string, inferHdr) {
	t.Helper()
	s.SetReadDeadline(time.Now().Add(within))
	defer s.SetReadDeadline(time.Time{})
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading a header line: %q, %v", line, err)
	}
	var h inferHdr
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		t.Fatalf("header line %q: %v", line, err)
	}
	return strings.TrimSpace(line), h
}

// servedBy starts a node sharing model ds from upstream.
func servedBy(t *testing.T, up *httptest.Server, cfg Config) *Node {
	t.Helper()
	cfg.Upstream = up.URL + "/v1"
	cfg.Models = map[string]string{"ds": "local-ds"}
	return testNode(t, t.Context(), cfg)
}

// captureLog collects what the node logs for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return logs
}

// TestEvictWorseClass: free ids that fill a server's queue do not shut a member
// out. The youngest of them loses its place and hears why on its own stream,
// its reservation goes back, and the accounting is right after the queue drains.
func TestEvictWorseClass(t *testing.T) {
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	root, memberKey := newEthKey(t), newEthKey(t)
	const daily = 1_000_000
	n := servedBy(t, up, Config{Roots: []string{root.addr}, Concurrency: 1, FreeDaily: daily, MaxTokens: 8})
	if !n.members.Add(root.invite(memberKey.addr, 1)) {
		t.Fatal("invite refused")
	}
	memberID, memberNode := claimNode(t, n, memberKey)
	now := time.Now()
	slot := takeSlot(t, n, randomPeer(t), now)

	// 8 free ids take every place, one stream at a time.
	type place struct {
		s  network.Stream
		br *bufio.Reader
	}
	var places []place
	var last host.Host
	for range maxQueue / maxPerPeerQueue {
		last = scriptedPeer(t, n, nil, "")
		for range maxPerPeerQueue {
			s := openInfer(t, last, n, protocol.InferProto2, chatReq(time.Minute), true)
			br := bufio.NewReader(s)
			if _, h := readHdr(t, br, s, 10*time.Second); h.Status != 202 {
				t.Fatalf("a free id was not queued: %+v", h)
			}
			places = append(places, place{s, br})
		}
	}
	if d := n.queueDepth(); d != maxQueue {
		t.Fatalf("queue depth %d, want %d", d, maxQueue)
	}
	youngest := places[len(places)-1]
	key := "node:" + last.ID().String()
	room := n.quota.Room(key, daily, now)

	// The member gets in.
	mh := scriptedPeer(t, n, memberNode, "")
	if mh.ID() != memberID {
		t.Fatal("wrong member id")
	}
	ms := openInfer(t, mh, n, protocol.InferProto2, chatReq(time.Minute), true)
	mbr := bufio.NewReader(ms)
	if _, h := readHdr(t, mbr, ms, 10*time.Second); h.Status != 202 {
		t.Fatalf("a member was refused a full queue of free ids: %+v", h)
	}
	// The youngest free id hears why; it may read a queue line first.
	for {
		_, h := readHdr(t, youngest.br, youngest.s, 10*time.Second)
		if h.Status == 202 {
			continue
		}
		if h.Status != 429 || h.Error != msgQueueFull {
			t.Fatalf("evicted waiter got %+v", h)
		}
		break
	}
	if d := n.queueDepth(); d != maxQueue {
		t.Fatalf("queue depth %d after the eviction, want %d", d, maxQueue)
	}
	if got := n.quota.Room(key, daily, now); got <= room {
		t.Fatalf("room %d after the eviction, was %d: the reservation did not come back", got, room)
	}

	// Everyone left runs in turn, member first, and the books balance.
	var wg sync.WaitGroup
	statuses := make(chan int, len(places))
	for _, p := range append(places[:len(places)-1:len(places)-1], place{ms, mbr}) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				p.s.SetReadDeadline(time.Now().Add(60 * time.Second))
				line, err := p.br.ReadString('\n')
				if err != nil {
					statuses <- -1
					return
				}
				var h inferHdr
				if json.Unmarshal([]byte(line), &h) != nil || h.Status != 202 {
					statuses <- h.Status
					return
				}
			}
		}()
	}
	n.done(slot, 0, now)
	wg.Wait()
	close(statuses)
	for st := range statuses {
		if st != 200 {
			t.Fatalf("a waiter ended with %d", st)
		}
	}
	eventually(t, 10*time.Second, "not idle after the queue drained", n.Idle)
	if n.inflight.Load() != 0 || n.openInflight.Load() != 0 || len(n.peerSlots) != 0 {
		t.Fatalf("slots leaked: %d %d %v", n.inflight.Load(), n.openInflight.Load(), n.peerSlots)
	}
}

// TestEvictRules: only a strictly better class evicts, the worst class goes
// first, and a peer over its own limit evicts nobody.
func TestEvictRules(t *testing.T) {
	n := testNode(t, t.Context(), Config{Concurrency: 1})
	now := time.Now()
	n.inflight.Add(1)
	defer n.inflight.Add(-1)
	var open *waiter
	for i := range maxQueue {
		class := classMember
		if i == 0 {
			class = classOpen
		}
		w := queueDirect(t, n, randomPeer(t), class, now.Add(time.Duration(i)*time.Millisecond))
		if i == 0 {
			open = w
		}
	}
	try := func(class int, p peer.ID) *waiter {
		n.qmu.Lock()
		defer n.qmu.Unlock()
		return n.evictForLocked(&waiter{peer: p, class: class, at: now})
	}
	if v := try(classMember, randomPeer(t)); v != open {
		t.Fatalf("a member evicted %+v, want the open waiter", v)
	}
	select {
	case <-open.evicted:
	default:
		t.Fatal("the evicted waiter was not told")
	}
	if n.cancelWait(open) != true {
		t.Fatal("an evicted waiter reported a grant")
	}
	n.qmu.Lock()
	n.enqueueLocked(&waiter{peer: randomPeer(t), class: classMember, at: now, ready: make(chan struct{})})
	n.qmu.Unlock()
	if v := try(classMember, randomPeer(t)); v != nil {
		t.Fatalf("a member evicted its equal: %+v", v)
	}
	if v := try(classContributor, randomPeer(t)); v == nil || v.class != classMember {
		t.Fatalf("a contributor evicted %+v", v)
	}
}

// TestQueueLimitByAccount: two node ids of one account share one queue limit.
func TestQueueLimitByAccount(t *testing.T) {
	root, owner := newEthKey(t), newEthKey(t)
	n := testNode(t, t.Context(), Config{Roots: []string{root.addr}, Concurrency: 1})
	if !n.members.Add(root.invite(owner.addr, 1)) {
		t.Fatal("invite refused")
	}
	first, _ := claimNode(t, n, owner)
	second, _ := claimNode(t, n, owner)
	now := time.Now()
	slot := takeSlot(t, n, randomPeer(t), now)
	defer n.done(slot, 0, now)
	for range maxPerPeerQueue {
		if s, msg, _ := n.admit(first, "m", now, time.Minute, 0); s != 202 {
			t.Fatalf("queued: %d %s", s, msg)
		}
	}
	if s, msg, _ := n.admit(second, "m", now, time.Minute, 0); s != 429 || msg != msgQueueFull {
		t.Fatalf("a second id of the same account: %d %s", s, msg)
	}
}
