package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
)

// startBudget is how long New may take in these tests. It takes milliseconds;
// the margin is for a loaded machine. Without the background join New never
// returns from a stalled seed, so a wider margin does not hide anything.
const startBudget = 5 * time.Second

// startNode is testNode with a time limit on New.
func startNode(t *testing.T, ctx context.Context, cfg Config) *Node {
	t.Helper()
	cfg.Listen = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.DataDir = t.TempDir()
	cfg.DHTServer = true
	cfg.AnnounceEvery = 300 * time.Millisecond
	cfg.Concurrency = 2
	type result struct {
		n   *Node
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := New(ctx, cfg)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		t.Cleanup(func() { r.n.host.Close(); r.n.closeState() })
		return r.n
	case <-time.After(startBudget):
		t.Fatalf("New did not return within %v", startBudget)
		return nil
	}
}

// quietSeed is a libp2p peer whose sync handler takes the stream and never
// answers. The test holds the streams and decides what happens to them.
type quietSeed struct {
	host    host.Host
	streams chan network.Stream
}

func newQuietSeed(t *testing.T) *quietSeed {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	q := &quietSeed{host: h, streams: make(chan network.Stream, 64)}
	h.SetStreamHandler(protocol.SyncProto, func(s network.Stream) { q.streams <- s })
	return q
}

func (q *quietSeed) addr() string {
	return q.host.Addrs()[0].Encapsulate(ma.StringCast("/p2p/" + q.host.ID().String())).String()
}

// nextSync waits for the next sync stream a node opens on this seed.
func (q *quietSeed) nextSync(t *testing.T) network.Stream {
	t.Helper()
	select {
	case s := <-q.streams:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the seed saw no sync")
		return nil
	}
}

// seedWithClaim is a real node holding a claim it does not gossip (claimLoop
// only gossips a node's own claim), so a peer can only get it by syncing.
func seedWithClaim(t *testing.T, ctx context.Context) (seed *Node, claimed string) {
	t.Helper()
	seed = testNode(t, ctx, Config{})
	id, _ := claimNode(t, seed, newEthKey(t))
	return seed, id.String()
}

func holdsClaim(n *Node, node string) bool {
	_, ok := n.claims.Get(node)
	return ok
}

// TestStalledSeedDoesNotHoldUpStartup: the updater and the gateway both start
// after New, so a seed that takes the sync stream and says nothing must not
// keep New from returning.
func TestStalledSeedDoesNotHoldUpStartup(t *testing.T) {
	up := mockUpstream(t, "local-ds")
	t.Cleanup(up.Close)
	seed := newQuietSeed(t)
	n := startNode(t, t.Context(), Config{Bootstrap: []string{seed.addr()}, Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds"}})

	// The sync is open and silent, with the default 30 s still to run.
	seed.nextSync(t)
	if w := post(n, `{"model":"ds","messages":[]}`); w.Code != 200 {
		t.Fatalf("gateway during a stalled sync: %d %s", w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	n.Gateway().ServeHTTP(w, gwReq("GET", "/status", nil))
	if w.Code != 200 {
		t.Fatalf("/status during a stalled sync: %d", w.Code)
	}
	if !n.Idle() {
		t.Fatal("a sync in progress must not keep the updater waiting")
	}
}

// syncWithin runs one sync on its own goroutine, so a sync that never returns
// fails the test instead of hanging it.
func syncWithin(t *testing.T, n *Node, p peer.ID, within time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := n.syncReceipts(t.Context(), p)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("the sync did not return within %v", within)
		return nil
	}
}

// TestStalledSyncTimesOut: silence for SyncStall is an error, and the seed sees
// the stream reset rather than left open.
func TestStalledSyncTimesOut(t *testing.T) {
	seed := newQuietSeed(t)
	n := startNode(t, t.Context(), Config{SyncStall: 500 * time.Millisecond})
	if err := n.host.Connect(t.Context(), peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if err := syncWithin(t, n, seed.host.ID(), 10*time.Second); err == nil {
		t.Fatal("a silent seed gave no error")
	}
	s := seed.nextSync(t)
	eventually(t, 5*time.Second, "the stream was not reset", func() bool {
		// Whitespace is not an item, and the sync is over anyway.
		_, err := s.Write([]byte("\n"))
		return errors.Is(err, network.ErrReset)
	})
}

// TestSyncLimit: a seed that keeps sending, just fast enough to never trip the
// read deadline, is still cut off.
func TestSyncLimit(t *testing.T) {
	seed := newQuietSeed(t)
	stopped := make(chan struct{})
	seed.host.SetStreamHandler(protocol.SyncProto, func(s network.Stream) {
		defer close(stopped)
		for {
			if _, err := s.Write([]byte("{}\n")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	n := startNode(t, t.Context(), Config{SyncLimit: time.Second})
	if err := n.host.Connect(t.Context(), peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if err := syncWithin(t, n, seed.host.ID(), 10*time.Second); err == nil {
		t.Fatal("an endless sync gave no error")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the seed was left writing to an open stream")
	}
}

// TestSlowSeedIsNotCutOff: the read deadline is per item, so a seed that takes
// longer than SyncStall overall but keeps delivering finishes its sync.
func TestSlowSeedIsNotCutOff(t *testing.T) {
	seed := newQuietSeed(t)
	stall := time.Second
	seed.host.SetStreamHandler(protocol.SyncProto, func(s network.Stream) {
		defer s.Close()
		for range 6 {
			if _, err := s.Write([]byte("{}\n")); err != nil {
				return
			}
			time.Sleep(stall / 4)
		}
	})
	n := startNode(t, t.Context(), Config{SyncStall: stall})
	if err := n.host.Connect(t.Context(), peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if err := syncWithin(t, n, seed.host.ID(), 10*time.Second); err != nil {
		t.Fatalf("a slow seed was cut off: %v", err)
	}
}

// TestStalledSeedLeavesOtherSeedsAlone: while one seed sits on its sync for the
// whole default deadline, another seed syncs right away.
func TestStalledSeedLeavesOtherSeedsAlone(t *testing.T) {
	ctx := t.Context()
	quiet := newQuietSeed(t)
	healthy, claimed := seedWithClaim(t, ctx)
	n := startNode(t, ctx, Config{Bootstrap: []string{quiet.addr(), healthy.Addrs()[0]}})
	quiet.nextSync(t)
	eventually(t, 10*time.Second, "the healthy seed did not sync", func() bool { return holdsClaim(n, claimed) })
}

// TestBlackHoleSeedDelaysNobody: an address that accepts the connection and
// never speaks holds a dial until it times out, which on loopback takes libp2p
// five seconds. The other seed's connect and sync must not wait for that.
func TestBlackHoleSeedDelaysNobody(t *testing.T) {
	ctx := t.Context()
	hole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hole.Close() })
	holeAddr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", hole.Addr().(*net.TCPAddr).Port, randomPeer(t))
	healthy, claimed := seedWithClaim(t, ctx)
	n := startNode(t, ctx, Config{Bootstrap: []string{holeAddr, healthy.Addrs()[0]}})
	eventually(t, 3*time.Second, "the healthy seed waited for the black hole", func() bool {
		return holdsClaim(n, claimed)
	})
}

// TestLateSeedGetsSynced: a seed that is down when the node starts is synced
// soon after it comes up, not at the next ten minute pass.
func TestLateSeedGetsSynced(t *testing.T) {
	ctx := t.Context()
	// Run the seed once for its identity and a claim on disk, so that the claim
	// is already there when the seed comes up on the address the node dials.
	dir := t.TempDir()
	seedCfg := func(listen string) Config {
		return Config{Listen: []string{listen}, DataDir: dir, DHTServer: true, AnnounceEvery: 300 * time.Millisecond}
	}
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	first, err := New(firstCtx, seedCfg("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := claimNode(t, first, newEthKey(t))
	claimed, seedID := id.String(), first.ID()
	first.host.Close()
	first.closeState()
	stopFirst()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := "/ip4/127.0.0.1/tcp/" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	redial := 100 * time.Millisecond
	n := startNode(t, ctx, Config{Bootstrap: []string{listen + "/p2p/" + seedID}, RedialEvery: redial})
	time.Sleep(3 * redial) // the first attempts find nothing listening

	seed, err := New(ctx, seedCfg(listen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seed.host.Close(); seed.closeState() })
	eventually(t, 15*time.Second, "the late seed was not synced", func() bool { return holdsClaim(n, claimed) })
}

// TestOneSyncPerSeedAtStartup: connecting and the first pass of the sync loop
// must not each ask a seed for its whole history.
func TestOneSyncPerSeedAtStartup(t *testing.T) {
	seed := newQuietSeed(t)
	redial := 20 * time.Millisecond
	startNode(t, t.Context(), Config{Bootstrap: []string{seed.addr()}, RedialEvery: redial})
	seed.nextSync(t).Close() // an empty history: the sync succeeded
	select {
	case <-seed.streams:
		t.Fatal("the seed was synced twice")
	case <-time.After(20 * redial):
	}
}

// TestFailedSyncIsRetried: a seed may refuse a sync while it is still starting,
// so a failure must not wait for the next ten minute pass.
func TestFailedSyncIsRetried(t *testing.T) {
	seed := newQuietSeed(t)
	startNode(t, t.Context(), Config{Bootstrap: []string{seed.addr()}, RedialEvery: 20 * time.Millisecond})
	seed.nextSync(t).Reset()
	seed.nextSync(t).Close()
}

// TestHungModelServerDoesNotHoldUpStartup: a model server that accepts GET
// /models and never answers must not block New, and the node still shares its
// models once the server answers.
func TestHungModelServerDoesNotHoldUpStartup(t *testing.T) {
	var hung atomic.Bool
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hung.CompareAndSwap(false, true) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	t.Cleanup(func() { close(release); up.Close() })
	n := startNode(t, t.Context(), Config{Upstream: up.URL + "/v1", ModelsTimeout: time.Second})
	if !hung.Load() {
		t.Fatal("New did not ask the model server")
	}
	eventually(t, 10*time.Second, "the node never picked up the model", func() bool { return n.ownModel("m") })
}

// TestJoiningNodeIsAdvertised: the DHT loops wait for the routing table. A
// lookup on an empty one fails, and the rendezvous advertise then waits two
// minutes before it tries again.
func TestJoiningNodeIsAdvertised(t *testing.T) {
	ctx := t.Context()
	up := mockUpstream(t, "local-ds")
	t.Cleanup(up.Close)
	seed := testNode(t, ctx, Config{})
	n := startNode(t, ctx, Config{Bootstrap: seed.Addrs()[:1], Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds"}})
	sum, err := multihash.Sum([]byte(protocol.Rendezvous), multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	for what, key := range map[string]cid.Cid{"rendezvous": cid.NewCidV1(cid.Raw, sum), "model": modelKey("ds")} {
		eventually(t, 10*time.Second, what+" record never reached the seed", func() bool {
			providers, _ := seed.dht.ProviderStore().GetProviders(ctx, key.Hash())
			for _, p := range providers {
				if p.ID == n.host.ID() {
					return true
				}
			}
			return false
		})
	}
}
