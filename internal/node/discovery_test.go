package node

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	ma "github.com/multiformats/go-multiaddr"
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
	// Not a public node, so no announce loop replaces the announce stored below.
	server, err := New(ctx, Config{Bootstrap: boot.Addrs()[:1], Listen: []string{"/ip4/127.0.0.1/tcp/0"},
		DataDir: t.TempDir(), AnnounceEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.host.Close() })
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})

	server.lastAnn.Store(&protocol.Announce{Version: Version, Models: []string{"m"}, Free: 1, Max: 1})
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
		deadline := time.Now().Add(5 * time.Second)
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
	var stopBoot context.CancelFunc
	startBoot := func() *Node {
		// Its own context, so the old node's loops stop before the new one
		// opens the same data directory and port.
		bctx, bcancel := context.WithCancel(ctx)
		var n *Node
		var err error
		for i := 0; i < 40; i++ { // the old listener may take a moment to go
			n, err = New(bctx, Config{Listen: []string{addr}, DataDir: dir, DHTServer: true,
				AnnounceEvery: 300 * time.Millisecond, Concurrency: 2})
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			bcancel()
			t.Fatal(err)
		}
		stopBoot = bcancel
		t.Cleanup(func() { n.host.Close(); bcancel() })
		return n
	}
	boot := startBoot()
	bs := boot.Addrs()[:1]
	natted := func(cfg Config) *Node {
		cfg.Bootstrap, cfg.Private, cfg.DataDir = bs, true, t.TempDir()
		cfg.AnnounceEvery, cfg.Concurrency = 300*time.Millisecond, 2
		cfg.RedialEvery = 200 * time.Millisecond
		n, err := New(ctx, cfg) // no Listen: unreachable except via relay
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
	waitServed("before the restart", 20*time.Second)

	// Take the bootstrap down, as a restart would, and bring it back with the
	// same identity on the same address.
	boot.host.Close()
	stopBoot()
	startBoot()
	waitServed("after the bootstrap restarted", 15*time.Second)

	relayed := false
	for _, c := range client.host.Network().ConnsToPeer(server.host.ID()) {
		relayed = relayed || strings.Contains(c.RemoteMultiaddr().String(), "/p2p-circuit")
	}
	if !relayed {
		t.Fatal("expected the answered connection to be relayed")
	}
}

// providerOf reports whether a lookup through n finds server for model.
func providerOf(ctx context.Context, n, server *Node, model string) (found bool) {
	fctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for ai := range n.dht.FindProvidersAsync(fctx, modelKey(model), 10) {
		found = found || ai.ID == server.host.ID()
	}
	return found
}

// TestProvideOnSeedRestart: a seed that restarted has lost its provider records.
// A host repairs them when the seed comes back, well before its periodic refresh,
// so a cold client finds the host soon after.
func TestProvideOnSeedRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	dir := t.TempDir()
	var stopBoot context.CancelFunc
	startBoot := func() *Node {
		bctx, bcancel := context.WithCancel(ctx)
		var n *Node
		var err error
		for i := 0; i < 40; i++ { // the old listener may take a moment to go
			n, err = New(bctx, Config{Listen: []string{addr}, DataDir: dir, DHTServer: true,
				AnnounceEvery: 300 * time.Millisecond, Concurrency: 2})
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			bcancel()
			t.Fatal(err)
		}
		stopBoot = bcancel
		t.Cleanup(func() { n.host.Close(); bcancel() })
		return n
	}
	boot := startBoot()
	bs := boot.Addrs()[:1]
	// With a listen address of its own: autorelay gives a NATed node no address on
	// a loopback relay, and a record with no address is not kept.
	server := testNode(t, ctx, Config{Bootstrap: bs, RedialEvery: 200 * time.Millisecond,
		Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	for deadline := time.Now().Add(20 * time.Second); !providerOf(ctx, boot, server, "ds"); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no provider record before the restart")
		}
	}

	boot.host.Close()
	stopBoot()
	startBoot()
	cold := testNode(t, ctx, Config{Bootstrap: bs})
	for deadline := time.Now().Add(15 * time.Second); !providerOf(ctx, cold, server, "ds"); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a cold client cannot find the host after the seed restarted")
		}
	}
}

// TestProvideFollowsAnnounce: a new model gets its record at once, nothing is
// provided while the upstream is down, and recovery provides again at once.
func TestProvideFollowsAnnounce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var down atomic.Bool
	mock := mockUpstream(t, "local-ds")
	defer mock.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		mock.Config.Handler.ServeHTTP(w, r)
	}))
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	// A short refresh would provide all the time if being down did not stop it.
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds"}, ProvideEvery: 100 * time.Millisecond})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	waitFor("no first provide", func() bool { return server.provides.Load() > 0 })

	down.Store(true)
	waitFor("the announce did not empty", func() bool { return len(server.announced()) == 0 })
	time.Sleep(300 * time.Millisecond) // lets a pass that was running finish
	at := server.provides.Load()
	time.Sleep(600 * time.Millisecond)
	if got := server.provides.Load(); got != at {
		t.Fatalf("provided %d times while the upstream was down", got-at)
	}

	down.Store(false)
	waitFor("no provide after recovery", func() bool { return server.provides.Load() > at })

	server.setModels(map[string]string{"ds": "local-ds", "ds2": "local-ds"})
	waitFor("no record for a new model", func() bool { return providerOf(ctx, client, server, "ds2") })
}

// TestProvideOnlyOnChange: with no reason to, a node does not provide again.
func TestProvideOnlyOnChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	if server.cfg.ProvideEvery != 30*time.Minute {
		t.Fatalf("default refresh = %v", server.cfg.ProvideEvery)
	}
	for deadline := time.Now().Add(20 * time.Second); server.provides.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no first provide")
		}
	}
	time.Sleep(time.Second) // passes asked for during startup finish
	at := server.provides.Load()
	time.Sleep(time.Second)
	if got := server.provides.Load(); got != at {
		t.Fatalf("provided %d more times with nothing changed", got-at)
	}
}

// TestProvideWaitsForAnAddress: a node with no address yet sends no record, since
// none would be kept, and a change of its addresses asks for a pass at once.
func TestProvideWaitsForAnAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	natted, err := New(ctx, Config{Bootstrap: boot.Addrs()[:1], Private: true, DataDir: t.TempDir(), AnnounceEvery: 300 * time.Millisecond,
		Concurrency: 2, Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { natted.host.Close() })
	for len(natted.announced()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if got := natted.provides.Load(); got != 0 || len(natted.host.Addrs()) != 0 {
		t.Fatalf("provided %d times with addresses %v", got, natted.host.Addrs())
	}

	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	for deadline := time.Now().Add(20 * time.Second); server.provides.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no first provide")
		}
	}
	time.Sleep(time.Second) // passes asked for during startup finish
	at := server.provides.Load()
	em, err := server.host.EventBus().Emitter(new(event.EvtLocalAddressesUpdated), eventbus.Stateful)
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	if err := em.Emit(event.EvtLocalAddressesUpdated{Diffs: true}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); server.provides.Load() == at; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a change of addresses did not ask for a provide")
		}
	}
}

// TestDeadProvidersDoNotBlockTheLookup: providers that accept a connection and
// never finish the handshake come first, and the live one behind them still
// gets answered within the lookup's budget.
func TestDeadProvidersDoNotBlockTheLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local-ds")
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Private: true})
	for server.lastAnn.Load() == nil {
		time.Sleep(10 * time.Millisecond)
	}

	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/" + strconv.Itoa(silent.Addr().(*net.TCPAddr).Port))
	found := make(chan peer.AddrInfo, findConcurrency)
	for range findConcurrency - 1 {
		found <- peer.AddrInfo{ID: randomPeer(t), Addrs: []ma.Multiaddr{addr}}
	}
	found <- peer.AddrInfo{ID: server.host.ID(), Addrs: server.host.Addrs()}
	close(found)

	lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
	defer lcancel()
	client.connectProviders(lctx, found)
	if got := client.candidates("ds"); len(got) != 1 || got[0] != server.host.ID() {
		t.Fatalf("candidates = %v", got)
	}
}
