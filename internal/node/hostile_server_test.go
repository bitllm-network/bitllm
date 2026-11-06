package node

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/bitllm-org/bitllm/internal/receipts"
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

// quicServed is servedBy over QUIC, the transport direct connections prefer.
func quicServed(t *testing.T, up *httptest.Server, cfg Config) *Node {
	t.Helper()
	cfg.Upstream = up.URL + "/v1"
	cfg.Models = map[string]string{"ds": "local-ds"}
	cfg.Listen = []string{"/ip4/127.0.0.1/udp/0/quic-v1"}
	cfg.DataDir = t.TempDir()
	cfg.DHTServer = true
	cfg.AnnounceEvery = 300 * time.Millisecond
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	n, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.host.Close() })
	return n
}

var transports = map[string]func(*testing.T, *httptest.Server, Config) *Node{"tcp": servedBy, "quic": quicServed}

// endlessUpstream streams until the request goes away, and says when it did.
func endlessUpstream(t *testing.T) (up *httptest.Server, gone <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 16)
	line := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 8<<10) + "\"}}]}\n\n"
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		defer func() { ch <- struct{}{} }()
		for r.Context().Err() == nil {
			if _, err := fmt.Fprint(w, line); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)
	return up, ch
}

func waitGone(t *testing.T, gone <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-gone:
	case <-time.After(30 * time.Second):
		t.Fatal(what)
	}
}

// TestPeerStopsReading: a peer that reads nothing loses the slot and stops the
// upstream job soon after the stream fills up.
func TestPeerStopsReading(t *testing.T) {
	setVar(t, &peerWriteStall, 300*time.Millisecond)
	for name, serve := range transports {
		t.Run(name, func(t *testing.T) {
			up, gone := endlessUpstream(t)
			n := serve(t, up, Config{Concurrency: 1})
			h := scriptedPeer(t, n, nil, "")
			openInfer(t, h, n, protocol.InferProto2, inferReq{Path: "/chat/completions",
				Body: json.RawMessage(`{"model":"ds","stream":true,"messages":[]}`)}, true)
			eventually(t, 10*time.Second, "the job never started", func() bool { return n.inflight.Load() == 1 })
			waitGone(t, gone, "the upstream job kept running for a peer that reads nothing")
			eventually(t, 10*time.Second, "the slot was not given back", n.Idle)
		})
	}
}

// TestSlowReaderKeepsSlot: the deadline covers each write, not the answer, so a
// slow reader that keeps reading gets all of it.
func TestSlowReaderKeepsSlot(t *testing.T) {
	setVar(t, &peerWriteStall, time.Second)
	answer := strings.Repeat("x", 1<<20)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, answer)
	}))
	defer up.Close()
	n := servedBy(t, up, Config{Concurrency: 1})
	h := scriptedPeer(t, n, nil, "")
	s := openInfer(t, h, n, protocol.InferProto2, chatReq(0), true)
	s.SetReadDeadline(time.Now().Add(60 * time.Second))
	start, got := time.Now(), 0
	buf := make([]byte, 64<<10)
	for {
		m, err := s.Read(buf)
		got += m
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("after %d bytes: %v", got, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got < len(answer) || time.Since(start) < time.Second {
		t.Fatalf("read %d bytes in %v", got, time.Since(start))
	}
}

// TestAllowRefusesUnread: an unlisted peer is refused at once, on a request it
// has not even sent, and a large one it is still writing does not hang it.
func TestAllowRefusesUnread(t *testing.T) {
	setVar(t, &refuseGrace, 200*time.Millisecond)
	for name, serve := range transports {
		t.Run(name, func(t *testing.T) {
			up := mockUpstream(t, "local-ds")
			defer up.Close()
			friend := randomPeer(t)
			n := serve(t, up, Config{Allow: map[peer.ID]bool{friend: true}})
			h := scriptedPeer(t, n, nil, "")
			s, err := h.NewStream(t.Context(), n.host.ID(), protocol.InferProto2)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Reset()
			if _, hdr := readHdr(t, bufio.NewReader(s), s, 5*time.Second); hdr.Status != 403 || !notAllowed(hdr.Status, hdr.Error) {
				t.Fatalf("%+v", hdr)
			}
			big, err := h.NewStream(t.Context(), n.host.ID(), protocol.InferProto2)
			if err != nil {
				t.Fatal(err)
			}
			defer big.Reset()
			big.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := big.Write(make([]byte, 8<<20)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("a large write to a server that refused it: %v", err)
			}
		})
	}
}

// halfRequest opens a stream and sends part of a request line, never the end.
func halfRequest(t *testing.T, h host.Host, n *Node, size int) network.Stream {
	t.Helper()
	s, err := h.NewStream(t.Context(), n.host.ID(), protocol.InferProto2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Reset() })
	if _, err := s.Write(append([]byte(`{"path":"`), make([]byte, size)...)); err != nil {
		t.Fatal(err)
	}
	return s
}

func gateState(n *Node) (readers int, bytes int64) {
	n.gate.mu.Lock()
	defer n.gate.mu.Unlock()
	return n.gate.readers, n.gate.bytes
}

// TestReaderCaps: requests being read are bounded per peer and overall, memory
// follows what was actually sent, and all of it comes back.
func TestReaderCaps(t *testing.T) {
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	n := servedBy(t, up, Config{})
	busy := func(s network.Stream) {
		t.Helper()
		if _, hdr := readHdr(t, bufio.NewReader(s), s, 5*time.Second); hdr.Status != 429 || !isBusy(hdr.Status, hdr.Error) {
			t.Fatalf("%+v", hdr)
		}
	}
	var open []network.Stream
	for range maxReaders / maxPeerReaders {
		h := scriptedPeer(t, n, nil, "")
		for range maxPeerReaders {
			open = append(open, halfRequest(t, h, n, 10))
			want := len(open)
			eventually(t, 10*time.Second, "reader not counted", func() bool { r, _ := gateState(n); return r == want })
		}
		if len(open) == maxPeerReaders {
			busy(halfRequest(t, h, n, 10)) // one peer, past its share
		}
	}
	busy(halfRequest(t, scriptedPeer(t, n, nil, ""), n, 10)) // the server, past its total
	if _, b := gateState(n); b != maxReaders*firstReadBuf {
		t.Fatalf("%d silent readers hold %d bytes: a buffer was preallocated", maxReaders, b)
	}

	// Memory grows with what arrives, not before.
	if _, err := open[0].Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "buffer did not grow", func() bool { _, b := gateState(n); return b >= 1<<20 })
	if _, b := gateState(n); b > 3<<20+maxReaders*firstReadBuf {
		t.Fatalf("1 MiB held %d bytes", b)
	}

	for _, s := range open {
		s.Reset()
	}
	eventually(t, 10*time.Second, "readers not given back", func() bool {
		r, b := gateState(n)
		return r == 0 && b == 0 && len(n.gate.perPeer) == 0
	})

	// A request past the limit is refused, not kept.
	oversized := halfRequest(t, scriptedPeer(t, n, nil, ""), n, 0)
	go oversized.Write(make([]byte, maxRequest+1))
	if _, hdr := readHdr(t, bufio.NewReader(oversized), oversized, 60*time.Second); hdr.Status != 400 {
		t.Fatalf("oversized: %+v", hdr)
	}
	eventually(t, 10*time.Second, "oversized request kept its bytes", func() bool { _, b := gateState(n); return b == 0 })
}

// TestBodyBudget: bodies being read and bodies waiting in the queue share one
// budget, which a job gives back when it starts or leaves.
func TestBodyBudget(t *testing.T) {
	setVar(t, &bodyBudget, 1<<20)
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		<-release
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
	}))
	defer up.Close()
	defer free()
	n := servedBy(t, up, Config{Concurrency: 1})
	now := time.Now()
	slot := takeSlot(t, n, randomPeer(t), now)
	big := func() inferReq {
		return inferReq{Path: "/chat/completions", Wait: time.Minute.Milliseconds(), Body: json.RawMessage(
			`{"model":"ds","messages":[{"role":"user","content":"` + strings.Repeat("x", 600<<10) + `"}]}`)}
	}
	first := openInfer(t, scriptedPeer(t, n, nil, ""), n, protocol.InferProto2, big(), true)
	firstR := bufio.NewReader(first)
	if _, hdr := readHdr(t, firstR, first, 10*time.Second); hdr.Status != 202 {
		t.Fatalf("first: %+v", hdr)
	}
	if _, b := gateState(n); b < 600<<10 || b > 700<<10 {
		t.Fatalf("a queued body holds %d bytes", b)
	}
	// It is refused while still writing, so that write cannot finish first.
	second, err := scriptedPeer(t, n, nil, "").NewStream(t.Context(), n.host.ID(), protocol.InferProto2)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Reset()
	line, _ := json.Marshal(big())
	go second.Write(append(line, '\n'))
	if _, hdr := readHdr(t, bufio.NewReader(second), second, 10*time.Second); hdr.Status != 429 || !isBusy(hdr.Status, hdr.Error) {
		t.Fatalf("second, over budget: %+v", hdr)
	}
	eventually(t, 10*time.Second, "refused body kept its bytes", func() bool { _, b := gateState(n); return b < 700<<10 })

	// The job starts: its bytes go back while it runs.
	n.done(slot, 0, now)
	eventually(t, 10*time.Second, "job did not start", func() bool { return n.inflight.Load() == 1 && n.queueDepth() == 0 })
	eventually(t, 10*time.Second, "a running job still holds its body", func() bool { _, b := gateState(n); return b == 0 })
	free()
	eventually(t, 10*time.Second, "not idle", n.Idle)

	// A waiter that leaves gives its bytes back too.
	slot = takeSlot(t, n, randomPeer(t), now)
	third := openInfer(t, scriptedPeer(t, n, nil, ""), n, protocol.InferProto2, big(), true)
	if _, hdr := readHdr(t, bufio.NewReader(third), third, 10*time.Second); hdr.Status != 202 {
		t.Fatalf("third: %+v", hdr)
	}
	third.Reset()
	eventually(t, 10*time.Second, "a waiter that left kept its bytes", func() bool { _, b := gateState(n); return b == 0 })
	n.done(slot, 0, now)
}

// TestJobIDCap: a long job id never becomes a map key.
func TestJobIDCap(t *testing.T) {
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	n := servedBy(t, up, Config{})
	h := scriptedPeer(t, n, nil, "")
	long := strings.Repeat("j", maxJobID+1)
	for job, known := range map[string]bool{long: false, strings.Repeat("j", maxJobID): true} {
		req := chatReq(0)
		req.Job = job
		s := openInfer(t, h, n, protocol.InferProto2, req, true)
		br := bufio.NewReader(s)
		if _, hdr := readHdr(t, br, s, 10*time.Second); hdr.Status != 200 {
			t.Fatalf("%+v", hdr)
		}
		io.Copy(io.Discard, br)
		eventually(t, 10*time.Second, "job did not end", n.Idle)
		sr := receipts.SignedReceipt{Receipt: receipts.Receipt{Job: job}}
		err := n.receipts.Countersign(&sr, h.ID())
		if got := !errors.Is(err, receipts.ErrUnknownJob); got != known {
			t.Fatalf("job of %d bytes recorded=%v, want %v (%v)", len(job), got, known, err)
		}
	}
}

// prefillUpstream sends nothing, not even headers, until told or canceled.
func prefillUpstream(t *testing.T, delay time.Duration) (up *httptest.Server, gone <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 16)
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
		case <-r.Context().Done():
			ch <- struct{}{}
		}
	}))
	t.Cleanup(up.Close)
	return up, ch
}

// TestCleanCloseWriteCancelsNothing: the CloseWrite every client sends is not a
// hang-up, however long the job then takes.
func TestCleanCloseWriteCancelsNothing(t *testing.T) {
	setVar(t, &peerGonePoll, 20*time.Millisecond)
	for name, serve := range transports {
		t.Run(name, func(t *testing.T) {
			up, gone := prefillUpstream(t, 500*time.Millisecond)
			n := serve(t, up, Config{})
			h := scriptedPeer(t, n, nil, "")
			s := openInfer(t, h, n, protocol.InferProto, chatReq(0), true)
			if _, hdr := readHdr(t, bufio.NewReader(s), s, 20*time.Second); hdr.Status != 200 {
				t.Fatalf("%+v", hdr)
			}
			select {
			case <-gone:
				t.Fatal("the upstream job was canceled")
			default:
			}
		})
	}
}

// TestHangUpDuringPrefill: a client that leaves while our server is still reading
// its prompt stops the upstream request, on each way a client can leave.
func TestHangUpDuringPrefill(t *testing.T) {
	setVar(t, &peerGonePoll, 20*time.Millisecond)
	setVar(t, &progressEvery, 50*time.Millisecond)
	for name, c := range map[string]struct {
		serve      func(*testing.T, *httptest.Server, Config) *Node
		proto      p2pproto.ID
		wait       time.Duration
		closeWrite bool
		leave      func(network.Stream, host.Host)
	}{
		"quic reset":             {quicServed, protocol.InferProto, 0, true, func(s network.Stream, _ host.Host) { s.Reset() }},
		"tcp reset, no FIN":      {servedBy, protocol.InferProto, 0, false, func(s network.Stream, _ host.Host) { s.Reset() }},
		"tcp reset, 202 lines":   {servedBy, protocol.InferProto2, time.Minute, true, func(s network.Stream, _ host.Host) { s.Reset() }},
		"tcp connection dropped": {servedBy, protocol.InferProto, 0, true, func(_ network.Stream, h host.Host) { h.Close() }},
	} {
		t.Run(name, func(t *testing.T) {
			up, gone := prefillUpstream(t, time.Hour)
			n := c.serve(t, up, Config{})
			h := scriptedPeer(t, n, nil, "")
			req := chatReq(c.wait)
			s := openInfer(t, h, n, c.proto, req, c.closeWrite)
			eventually(t, 10*time.Second, "job never started", func() bool { return n.inflight.Load() == 1 })
			time.Sleep(100 * time.Millisecond)
			c.leave(s, h)
			waitGone(t, gone, "the upstream request kept running")
			eventually(t, 10*time.Second, "slot not given back", n.Idle)
		})
	}
}

// TestHangUpStopsAtFirstToken: over yamux a reset after CloseWrite shows only on
// a write, so a streaming job that already said 200 stops at its first token.
func TestHangUpStopsAtFirstToken(t *testing.T) {
	up, gone := endlessUpstream(t)
	n := servedBy(t, up, Config{})
	h := scriptedPeer(t, n, nil, "")
	s := openInfer(t, h, n, protocol.InferProto, inferReq{Path: "/chat/completions",
		Body: json.RawMessage(`{"model":"ds","stream":true,"messages":[]}`)}, true)
	if _, hdr := readHdr(t, bufio.NewReader(s), s, 10*time.Second); hdr.Status != 200 {
		t.Fatalf("%+v", hdr)
	}
	s.Reset()
	waitGone(t, gone, "the upstream job kept running")
	eventually(t, 10*time.Second, "slot not given back", n.Idle)
}

// TestQuotaFloor: a stream that ends with no usage reported pays for its prompt
// by body size, and what the signed receipt counts does not change.
func TestQuotaFloor(t *testing.T) {
	up, gone := endlessUpstream(t)
	const daily = 1_000_000
	n := servedBy(t, up, Config{FreeDaily: daily})
	h := scriptedPeer(t, n, nil, "")
	body := `{"model":"ds","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 16_000) + `"}]}`
	s := openInfer(t, h, n, protocol.InferProto, inferReq{Path: "/chat/completions", Body: json.RawMessage(body)}, true)
	if _, hdr := readHdr(t, bufio.NewReader(s), s, 10*time.Second); hdr.Status != 200 {
		t.Fatalf("%+v", hdr)
	}
	s.Reset()
	waitGone(t, gone, "the upstream job kept running")
	eventually(t, 10*time.Second, "slot not given back", n.Idle)
	key, now := "node:"+h.ID().String(), time.Now()
	used, served := n.quota.Used(key, now), -n.ledger.Balance(h.ID())
	if floor := int64(len(body)) / 16; used != floor+served {
		t.Fatalf("charged %d, want the %d floor plus the %d the job metered", used, floor, served)
	}
	if room := n.quota.Room(key, daily, now); room != daily-used {
		t.Fatalf("room %d, want %d: a reservation was kept", room, daily-used)
	}

	// The receipt side is plain metering, floor or no floor.
	var m meter
	m.line([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"12345678\"}}]}\n"))
	if m.credits() != 2 || quotaCredits(&m, 1600, true) != 102 || quotaCredits(&m, 1600, false) != 2 {
		t.Fatalf("credits %d, quota %d, %d", m.credits(), quotaCredits(&m, 1600, true), quotaCredits(&m, 1600, false))
	}
	m.usage(&usage{Prompt: 4, Completion: 1})
	if quotaCredits(&m, 1600, true) != m.credits() {
		t.Fatal("a reported usage was not taken as it is")
	}
}

// TestFailedUpstreamChargesNothing: our own server's failure costs the peer no
// quota beyond what it was served.
func TestFailedUpstreamChargesNothing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		http.Error(w, "broken", 500)
	}))
	defer up.Close()
	n := servedBy(t, up, Config{FreeDaily: 1000})
	h := scriptedPeer(t, n, nil, "")
	if hdr := askPeer(t, h, n, "/chat/completions", `{"model":"ds","messages":[]}`); hdr.Status != 502 {
		t.Fatalf("%+v", hdr)
	}
	eventually(t, 10*time.Second, "slot not given back", n.Idle)
	if used := n.quota.Used("node:"+h.ID().String(), time.Now()); used != 0 {
		t.Fatalf("charged %d", used)
	}
}

// TestProgressLines: a job waiting for our server's headers says so on the wire,
// only to a client that may queue, and nothing lands after the 200.
func TestProgressLines(t *testing.T) {
	setVar(t, &progressEvery, 30*time.Millisecond)
	up, _ := prefillUpstream(t, 300*time.Millisecond)
	n := servedBy(t, up, Config{})
	h := scriptedPeer(t, n, nil, "")

	s := openInfer(t, h, n, protocol.InferProto2, chatReq(time.Minute), true)
	s.SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 4 { // two 202 lines at least, the 200, the answer
		t.Fatalf("lines: %q", lines)
	}
	end := len(lines) - 2
	for _, l := range lines[:end] {
		if l != `{"status":202}` {
			t.Fatalf("expected a progress line, got %q in %q", l, lines)
		}
	}
	if lines[end] != `{"status":200}` || !strings.HasPrefix(lines[end+1], `{"choices"`) {
		t.Fatalf("the end of the stream: %q", lines[end:])
	}

	// A client that did not offer to wait, or an old protocol, never sees one.
	for proto, wait := range map[p2pproto.ID]time.Duration{protocol.InferProto2: 0, protocol.InferProto: time.Minute} {
		s := openInfer(t, h, n, proto, chatReq(wait), true)
		s.SetReadDeadline(time.Now().Add(30 * time.Second))
		raw, err := io.ReadAll(s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(raw), `{"status":200}`) {
			t.Fatalf("%s wait %v: %q", proto, wait, raw)
		}
	}
}
