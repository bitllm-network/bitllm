package node

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func TestGatewayRefusesRebinding(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{})
	for host, want := range map[string]int{
		"127.0.0.1:7778": 200, "localhost:7778": 200, "[::1]:7778": 200, "192.168.1.5:7778": 200,
		"evil.example:7778": 403, "localhost.evil.example": 403,
	} {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		n.Gateway().ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
	// Same Origin and Host, as a rebinding page sends, is still refused.
	r := httptest.NewRequest("POST", "/account/link", nil)
	r.Host = "evil.example:7778"
	r.Header.Set("Origin", "http://evil.example:7778")
	w := httptest.NewRecorder()
	n.Gateway().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("rebinding POST: %d", w.Code)
	}
}

func TestClamp(t *testing.T) {
	n := &Node{cfg: Config{MaxTokens: 1000}}
	b := map[string]any{"n": 8.0, "best_of": 8.0, "max_tokens": 1e9}
	n.clamp("/chat/completions", b)
	if _, ok := b["n"]; ok || b["max_tokens"] != 1000.0 {
		t.Fatalf("chat: %v", b)
	}
	b = map[string]any{}
	n.clamp("/messages", b)
	if b["max_tokens"] != 1000.0 {
		t.Fatalf("default cap: %v", b)
	}
	b = map[string]any{"max_tokens": 50.0}
	n.clamp("/messages", b)
	if b["max_tokens"] != 50.0 {
		t.Fatalf("lower request changed: %v", b)
	}
	b = map[string]any{}
	n.clamp("/completions", b)
	if _, ok := b["max_tokens"]; ok {
		t.Fatalf("legacy completions default changed: %v", b)
	}
}

// TestBootstrapAddrsMergedPerPeer: tcp and quic addresses of one bootstrap
// node are one peer, used once as a relay.
func TestBootstrapAddrsMergedPerPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boot, err := New(ctx, Config{Listen: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
		DataDir: t.TempDir(), DHTServer: true, AnnounceEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer boot.host.Close()
	addrs := boot.Addrs()
	if len(addrs) < 2 {
		t.Fatalf("boot addrs %v", addrs)
	}
	n := testNode(t, ctx, Config{Bootstrap: addrs})
	if len(n.relays) != 1 || len(n.relays[0].Addrs) != len(addrs) {
		t.Fatalf("relays %+v", n.relays)
	}
}

// TestRelayBehindProxyReservations: behind a tunnel every peer reaches the
// bootstrap from 127.0.0.1, so the default 8 reservations per IP would leave
// NATed node number 9 unreachable. A bootstrap marked RelayBehindProxy must
// hold more than 8.
func TestRelayBehindProxyReservations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boot := testNode(t, ctx, Config{RelayBehindProxy: true})
	bs := boot.Addrs()[:1]
	const n = 11
	var natted []*Node
	for range n {
		nd, err := New(ctx, Config{Bootstrap: bs, Private: true, DataDir: t.TempDir(), AnnounceEvery: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { nd.host.Close() })
		natted = append(natted, nd)
	}
	// The client accepts no inbound connections either, so the relay is the only path.
	client, err := New(ctx, Config{Bootstrap: bs, Private: true, DataDir: t.TempDir(), AnnounceEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.host.Close() })
	reached := map[int]bool{}
	for deadline := time.Now().Add(30 * time.Second); len(reached) < n; {
		for i, nd := range natted {
			if reached[i] {
				continue
			}
			client.addRelayPaths(nd.host.ID())
			cctx, c := context.WithTimeout(ctx, 2*time.Second)
			if client.host.Connect(cctx, peer.AddrInfo{ID: nd.host.ID()}) == nil {
				reached[i] = true
			}
			c()
		}
		if len(reached) < n && time.Now().After(deadline) {
			t.Fatalf("reached %d of %d NATed nodes through the relay", len(reached), n)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
