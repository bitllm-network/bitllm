package node

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	madns "github.com/multiformats/go-multiaddr-dns"
)

// changingDNS is a resolver whose answer a test can change while nodes run.
type changingDNS struct {
	mu  sync.Mutex
	txt []string
	err error
}

func (d *changingDNS) set(txt []string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.txt, d.err = txt, err
}

func (d *changingDNS) LookupTXT(context.Context, string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.txt, d.err
}

func (d *changingDNS) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return nil, nil
}

func withChangingDNS(t *testing.T) *changingDNS {
	t.Helper()
	d := &changingDNS{}
	r, err := madns.NewResolver(madns.WithDefaultResolver(d))
	if err != nil {
		t.Fatal(err)
	}
	old := bootstrapResolver
	bootstrapResolver = r
	t.Cleanup(func() { bootstrapResolver = old })
	return d
}

func seedIDs(n *Node) map[peer.ID]bool {
	out := map[peer.ID]bool{}
	for _, ai := range n.Seeds() {
		out[ai.ID] = true
	}
	return out
}

// TestRefreshSeeds: a seed added to DNS is connected and synced without a
// restart. A DNS failure keeps the list, including when another value on the
// list still answers. A seed taken out of DNS leaves the list, and the node's
// own id never joins it.
func TestRefreshSeeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seedA := testNode(t, ctx, Config{})
	literal := testNode(t, ctx, Config{})
	seedB := newQuietSeed(t) // shows the sync stream a new seed gets
	dnsaddr := func(n *Node) string { return "dnsaddr=" + n.Addrs()[0] }
	dnsaddrB := "dnsaddr=" + seedB.addr()

	dns := withChangingDNS(t)
	dns.set([]string{dnsaddr(seedA)}, nil)
	client := testNode(t, ctx, Config{Bootstrap: []string{"/dnsaddr/seeds.test", literal.Addrs()[0]}, RedialEvery: 200 * time.Millisecond})
	if got := seedIDs(client); len(got) != 2 || !got[seedA.host.ID()] || !got[literal.host.ID()] {
		t.Fatalf("seeds at start: %v", got)
	}

	dns.set([]string{dnsaddr(seedA), dnsaddrB}, nil)
	client.refreshSeeds(ctx)
	if got := seedIDs(client); len(got) != 3 || !got[seedB.host.ID()] {
		t.Fatalf("seeds after adding one: %v", got)
	}
	seedB.nextSync(t)

	// redialLoop reads the new list: losing the new seed brings it back.
	if err := client.host.Network().ClosePeer(seedB.host.ID()); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); client.host.Network().Connectedness(seedB.host.ID()) != network.Connected; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the new seed was not redialed")
		}
	}

	// DNS fails while the literal still resolves: the list stays whole.
	dns.set(nil, errors.New("no such host"))
	client.refreshSeeds(ctx)
	if got := seedIDs(client); len(got) != 3 {
		t.Fatalf("seeds after a DNS failure: %v", got)
	}

	// DNS answers without seedB, and with our own id.
	dns.set([]string{dnsaddr(seedA), dnsaddr(client)}, nil)
	client.refreshSeeds(ctx)
	got := seedIDs(client)
	if len(got) != 2 || got[seedB.host.ID()] || got[client.host.ID()] {
		t.Fatalf("seeds after removing one: %v", got)
	}
}

// TestRefreshSeedsNeverEmpties: with only a DNS name configured, a failure keeps
// the list instead of leaving the node with no seed.
func TestRefreshSeedsNeverEmpties(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seed := testNode(t, ctx, Config{})
	dns := withChangingDNS(t)
	dns.set([]string{"dnsaddr=" + seed.Addrs()[0]}, nil)
	client := testNode(t, ctx, Config{Bootstrap: []string{"/dnsaddr/seeds.test"}})

	dns.set(nil, errors.New("no such host"))
	client.refreshSeeds(ctx)
	if got := seedIDs(client); len(got) != 1 || !got[seed.host.ID()] {
		t.Fatalf("seeds after a DNS failure: %v", got)
	}
}
