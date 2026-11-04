package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestAllowAdmit: with --allow, listed peers get any free slot like members,
// and every other peer is refused for good, with its own id in the message.
func TestAllowAdmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := newEthKey(t)
	friend, stranger := randomPeer(t), randomPeer(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}, Concurrency: 2,
		Allow: map[peer.ID]bool{friend: true}})
	now := time.Now()

	// Not invited, but listed: served on both slots, not only when idle.
	s1, _, adm := n.admit(friend, "", now, 0, 0)
	t1 := adm.t
	s2, _, adm := n.admit(friend, "", now, 0, 0)
	t2 := adm.t
	if s1 != 0 || s2 != 0 {
		t.Fatalf("listed peer refused a free slot: %d %d", s1, s2)
	}
	n.done(t1, 0, now)
	n.done(t2, 0, now)

	s, msg, _ := n.admit(stranger, "", now, 0, 0)
	if s != 403 || !notAllowed(s, msg) || isBusy(s, msg) || !strings.Contains(msg, stranger.String()) {
		t.Fatalf("unlisted peer: %d %s", s, msg)
	}
	if n.inflight.Load() != 0 {
		t.Fatalf("slots leaked: %d", n.inflight.Load())
	}
}

// TestAllowNetwork: a client not on a server's list is not left waiting. It
// goes straight to another server for the model, and when the only server
// refuses it, it gets the refusal at once instead of a minute of queueing.
func TestAllowNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	friend := testNode(t, ctx, Config{Bootstrap: bs})
	stranger := testNode(t, ctx, Config{Bootstrap: bs})
	testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds", "solo": "local-ds"},
		Allow:  map[peer.ID]bool{friend.host.ID(): true}})
	public := testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds"}, Concurrency: 1})
	for deadline := time.Now().Add(20 * time.Second); len(stranger.candidates("ds")) < 2 ||
		len(stranger.candidates("solo")) == 0 || len(friend.candidates("solo")) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no discovery")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Two servers for ds: whichever the stranger tries first, the public one answers.
	w := post(stranger, `{"model":"ds","messages":[]}`)
	if w.Code != 200 || w.Header().Get("X-Bitllm-Peer") != public.host.ID().String() {
		t.Fatalf("stranger on ds: %d %s %s", w.Code, w.Header().Get("X-Bitllm-Peer"), w.Body)
	}

	// Only the private server has solo: a clear 403 right away.
	start := time.Now()
	w = post(stranger, `{"model":"solo","messages":[]}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "not allowed") ||
		!strings.Contains(w.Body.String(), stranger.host.ID().String()) {
		t.Fatalf("stranger on solo: %d %s", w.Code, w.Body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("refusal took %v; it must not wait like a busy server", d)
	}

	if w := post(friend, `{"model":"solo","messages":[]}`); w.Code != 200 {
		t.Fatalf("listed peer: %d %s", w.Code, w.Body)
	}

	// The public server is full for a moment: the private server's refusal
	// must not cut the busy wait short.
	public.inflight.Add(1) // its only slot
	go func() {
		time.Sleep(2 * time.Second)
		public.inflight.Add(-1)
	}()
	if w := post(stranger, `{"model":"ds","messages":[]}`); w.Code != 200 {
		t.Fatalf("stranger while the public server was busy: %d %s", w.Code, w.Body)
	}
}
