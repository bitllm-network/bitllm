package main

import (
	"context"
	"crypto/sha256"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multihash"
)

// TestModelKey: one DHT key per model name, derived the same on every node.
func TestModelKey(t *testing.T) {
	a := modelKey("qwen3-8b")
	if !a.Equals(modelKey("qwen3-8b")) {
		t.Fatal("model key is not stable")
	}
	if a.Equals(modelKey("qwen3-8b-elsewhere")) {
		t.Fatal("two models share a key")
	}
	sum := sha256.Sum256([]byte("bitllm/model/qwen3-8b"))
	mh, err := multihash.Sum(sum[:], multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	if want := cid.NewCidV1(cid.Raw, mh); !a.Equals(want) {
		t.Fatalf("model key = %s, want %s", a, want)
	}
}

// TestHelloFillsCandidates: a node found through the DHT is usable at once,
// without waiting for its next announce.
func TestHelloFillsCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boot := testNode(t, ctx, Config{})
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})

	server.lastAnn.Store(&Announce{Version: Version, Models: []string{"m"}, Free: 1, Max: 1})
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: server.host.ID(), Addrs: server.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	client.hello(ctx, server.host.ID())
	if got := client.candidates("m"); len(got) != 1 || got[0] != server.host.ID() {
		t.Fatalf("hello did not make the server usable: %v", got)
	}
}

// TestProviderRecords: a serving node records itself as a provider of each
// model, and another node finds it by model name alone.
func TestProviderRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds"}})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})

	deadline := time.Now().Add(30 * time.Second)
	for {
		found := false
		fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
		for ai := range client.dht.FindProvidersAsync(fctx, modelKey("ds"), 10) {
			if ai.ID == server.host.ID() {
				found = true
			}
		}
		fcancel()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never appeared as a provider of ds")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The cold-start path: the model name is enough to reach the server and
	// learn its announce, with no gossip involved.
	client.findModelPeers(ctx, "ds")
	if got := client.candidates("ds"); len(got) != 1 || got[0] != server.host.ID() {
		t.Fatalf("findModelPeers did not reach the provider: %v", got)
	}
}

// TestRedialReconnects: after losing a bootstrap, a node gets it back on its
// own instead of waiting for autorelay's backoff (issue #17).
func TestRedialReconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boot := testNode(t, ctx, Config{})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], RedialEvery: 200 * time.Millisecond})

	waitConnected := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			got := client.host.Network().Connectedness(boot.host.ID()) == network.Connected
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("connectedness = %v, want %v", got, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitConnected(true)
	if err := client.host.Network().ClosePeer(boot.host.ID()); err != nil {
		t.Fatal(err)
	}
	waitConnected(false)
	waitConnected(true) // redialLoop
}

// TestBootstrapRestart: a relayed-only network recovers on its own after the
// bootstrap restarts with the same identity, instead of waiting for autorelay's
// backoff (issue #17).
func TestBootstrapRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()

	// A fixed port, so the restarted bootstrap answers at the address the
	// client was given at startup.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	dir := t.TempDir()
	startBoot := func() *Node {
		var n *Node
		var err error
		for i := 0; i < 40; i++ { // the old listener may take a moment to go
			n, err = NewNode(ctx, Config{Listen: []string{addr}, DataDir: dir, DHTServer: true,
				AnnounceEvery: 300 * time.Millisecond, Concurrency: 2})
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.host.Close() })
		return n
	}
	boot := startBoot()
	bs := boot.Addrs()[:1]
	natted := func(cfg Config) *Node {
		cfg.Bootstrap, cfg.Private, cfg.DataDir = bs, true, t.TempDir()
		cfg.AnnounceEvery, cfg.Concurrency = 300*time.Millisecond, 2
		cfg.RedialEvery = 200 * time.Millisecond
		n, err := NewNode(ctx, cfg) // no Listen: unreachable except via relay
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.host.Close() })
		return n
	}
	server := natted(Config{Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	client := natted(Config{})

	served := func() bool {
		w := post(client, `{"model":"ds","stream":true,"messages":[]}`)
		return w.Code == 200 && strings.Contains(w.Body.String(), "[DONE]")
	}
	waitServed := func(what string, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			if served() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("no answer through the relay %s", what)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	waitServed("before the restart", 30*time.Second)

	// Take the bootstrap down, as a restart would, and bring it back with the
	// same identity on the same address.
	boot.host.Close()
	boot = startBoot()
	waitServed("after the bootstrap restarted", 30*time.Second)

	relayed := false
	for _, c := range client.host.Network().ConnsToPeer(server.host.ID()) {
		relayed = relayed || strings.Contains(c.RemoteMultiaddr().String(), "/p2p-circuit")
	}
	if !relayed {
		t.Fatal("expected the answered connection to be relayed")
	}
}
