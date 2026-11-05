package node

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
)

const (
	testSeedA = "12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib"
	testSeedB = "12D3KooWB3zAyx9hjQ47jKJp5Yyxzcgzw3dAd7ZExdiaY8NPFw2o"
)

type fakeDNS struct {
	txt    map[string][]string
	txtErr map[string]error
	ip     map[string][]net.IPAddr
}

func (f fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if err := f.txtErr[name]; err != nil {
		return nil, err
	}
	return f.txt[name], nil
}

func (f fakeDNS) LookupIPAddr(_ context.Context, name string) ([]net.IPAddr, error) {
	return f.ip[name], nil
}

func withFakeDNS(t *testing.T, f fakeDNS) {
	t.Helper()
	r, err := madns.NewResolver(madns.WithDefaultResolver(f))
	if err != nil {
		t.Fatal(err)
	}
	old := bootstrapResolver
	bootstrapResolver = r
	t.Cleanup(func() { bootstrapResolver = old })
}

func mustPeerID(t *testing.T, s string) peer.ID {
	t.Helper()
	id, err := peer.Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestResolveBootstrapDNSSeed(t *testing.T) {
	idA := mustPeerID(t, testSeedA)
	idB := mustPeerID(t, testSeedB)
	withFakeDNS(t, fakeDNS{txt: map[string][]string{
		"_dnsaddr.seeds.test": {
			"dnsaddr=/ip4/192.0.2.10/tcp/4001/p2p/" + testSeedA,
			"dnsaddr=/ip4/192.0.2.10/udp/4001/quic-v1/p2p/" + testSeedA,
			"dnsaddr=/ip4/192.0.2.11/tcp/4001/p2p/" + testSeedB,
		},
	}})

	infos, err := ResolveBootstrap(context.Background(), []string{"/dnsaddr/seeds.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d peers, want 2: %+v", len(infos), infos)
	}
	byID := map[peer.ID]int{}
	for _, ai := range infos {
		byID[ai.ID] = len(ai.Addrs)
	}
	if byID[idA] != 2 || byID[idB] != 1 {
		t.Fatalf("addresses per peer = %v, want %s:2 %s:1", byID, idA, idB)
	}
}

func TestResolveBootstrapDNSNameFiltersPeer(t *testing.T) {
	idA := mustPeerID(t, testSeedA)
	withFakeDNS(t, fakeDNS{txt: map[string][]string{
		"_dnsaddr.seeds.test": {
			"dnsaddr=/ip4/192.0.2.10/tcp/4001/p2p/" + testSeedA,
			"dnsaddr=/ip4/192.0.2.11/tcp/4001/p2p/" + testSeedB,
		},
	}})

	infos, err := ResolveBootstrap(context.Background(), []string{"/dnsaddr/seeds.test/p2p/" + testSeedA})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != idA {
		t.Fatalf("got %+v, want only %s", infos, idA)
	}
}

func TestResolveBootstrapSkipsRecordWithoutPeerID(t *testing.T) {
	withFakeDNS(t, fakeDNS{txt: map[string][]string{
		"_dnsaddr.seeds.test": {
			"dnsaddr=/ip4/192.0.2.12/tcp/4001",
			"dnsaddr=/ip4/192.0.2.10/tcp/4001/p2p/" + testSeedA,
		},
	}})

	infos, err := ResolveBootstrap(context.Background(), []string{"/dnsaddr/seeds.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID.String() != testSeedA {
		t.Fatalf("got %+v, want only %s", infos, testSeedA)
	}
}

func TestResolveBootstrapPlainMultiaddr(t *testing.T) {
	infos, err := ResolveBootstrap(context.Background(), []string{
		"/ip4/192.0.2.10/tcp/4001/p2p/" + testSeedA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || len(infos[0].Addrs) != 1 {
		t.Fatalf("got %+v, want one address", infos)
	}
	if _, err := ResolveBootstrap(context.Background(), []string{"/ip4/192.0.2.10/tcp/4001"}); err == nil {
		t.Fatal("plain multiaddr without a peer id was accepted")
	}
}

func TestResolveBootstrapDNSFailureIsNotFatal(t *testing.T) {
	withFakeDNS(t, fakeDNS{txtErr: map[string]error{
		"_dnsaddr.seeds.test": errors.New("no such host"),
	}})

	infos, err := ResolveBootstrap(context.Background(), []string{"/dnsaddr/seeds.test"})
	if err != nil {
		t.Fatalf("a failed DNS seed should not stop startup: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("got %+v, want no peers", infos)
	}
}

func TestHasDNSAddr(t *testing.T) {
	if !hasDNSAddr(ma.StringCast("/dnsaddr/seeds.test")) {
		t.Fatal("/dnsaddr/ not recognized")
	}
	if hasDNSAddr(ma.StringCast("/dns4/seeds.test/tcp/4001")) {
		t.Fatal("/dns4/ treated as a seed list")
	}
}

// TestClientReservesOnEveryBootstrap: a client with two seeds must keep a
// relay reservation on both, not just the first one. That is what makes a
// seed list resilient: losing one seed leaves the other reservation.
func TestClientReservesOnEveryBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	b1 := testNode(t, ctx, Config{})
	b2 := testNode(t, ctx, Config{})
	client, err := New(ctx, Config{
		Bootstrap:     append(b1.Addrs(), b2.Addrs()...),
		Private:       true,
		DataDir:       t.TempDir(),
		Listen:        []string{"/ip4/127.0.0.1/tcp/0"},
		AnnounceEvery: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.host.Close() })

	// A third node reaches the client through each seed. A hop to the client
	// only succeeds while the client holds a reservation on that seed, so this
	// checks the reservations themselves and not how libp2p advertises them.
	probe := testNode(t, ctx, Config{})
	for i, seed := range []*Node{b1, b2} {
		addrs := seed.host.Addrs()
		if len(addrs) == 0 {
			t.Fatalf("seed %d has no addresses", i+1)
		}
		a := addrs[0].Encapsulate(ma.StringCast("/p2p/" + seed.host.ID().String() + "/p2p-circuit/p2p/" + client.host.ID().String()))
		deadline := time.Now().Add(15 * time.Second)
		for {
			cctx, c := context.WithTimeout(ctx, 3*time.Second)
			err := probe.host.Connect(cctx, peer.AddrInfo{ID: client.host.ID(), Addrs: []ma.Multiaddr{a}})
			c()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no reservation on seed %d (%s): %v", i+1, a, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}
