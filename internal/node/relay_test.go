package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	circuitclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

func newPeerID(t *testing.T) peer.ID {
	t.Helper()
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func relayAddr(t *testing.T, id peer.ID) peer.AddrInfo {
	t.Helper()
	return peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/192.0.2.10/tcp/4001")}}
}

// TestRelaySource: a NATed node offers the seed and a public peer to
// autorelay, skips peers that only serve, and skips itself.
func TestRelaySource(t *testing.T) {
	seed := relayAddr(t, newPeerID(t))
	public := relayAddr(t, newPeerID(t))
	serving := relayAddr(t, newPeerID(t))
	self := newPeerID(t)
	n := &Node{
		cfg:    Config{AnnounceEvery: time.Second},
		self:   self,
		relays: []peer.AddrInfo{seed},
		peers: map[peer.ID]*peerInfo{
			public.ID:  {Announce: protocol.Announce{Relay: true}, seen: time.Now(), addrs: public.Addrs},
			serving.ID: {Announce: protocol.Announce{}, seen: time.Now(), addrs: serving.Addrs},
			self:       {Announce: protocol.Announce{Relay: true}, seen: time.Now(), addrs: relayAddr(t, self).Addrs},
		},
	}

	ch := n.relaySource(context.Background(), 2)
	var got []peer.ID
	for ai := range ch {
		got = append(got, ai.ID)
	}
	if len(got) != 2 || got[0] != seed.ID || got[1] != public.ID {
		t.Fatalf("relay source = %v, want [%s %s]", got, seed.ID, public.ID)
	}

	// A stale announcement is not a candidate.
	n.peers[public.ID].seen = time.Now().Add(-time.Hour)
	ch = n.relaySource(context.Background(), 2)
	got = got[:0]
	for ai := range ch {
		got = append(got, ai.ID)
	}
	if len(got) != 1 || got[0] != seed.ID {
		t.Fatalf("relay source after staleness = %v, want [%s]", got, seed.ID)
	}
}

// TestRelayInfosCapsAndDedupes: circuit paths use seeds first, then public
// relays, once each.
func TestRelayInfosCapsAndDedupes(t *testing.T) {
	seed := relayAddr(t, newPeerID(t))
	public := relayAddr(t, newPeerID(t))
	n := &Node{
		cfg:    Config{AnnounceEvery: time.Second},
		relays: []peer.AddrInfo{seed, seed},
		peers: map[peer.ID]*peerInfo{
			public.ID: {Announce: protocol.Announce{Relay: true}, seen: time.Now(), addrs: public.Addrs},
		},
	}
	got := n.relayInfos(4)
	if len(got) != 2 || got[0].ID != seed.ID || got[1].ID != public.ID {
		t.Fatalf("relayInfos = %+v, want [%s %s]", got, seed.ID, public.ID)
	}
}

// relayedPair starts a seed and two NATed nodes, one serving model with up, that
// can only reach each other through the seed.
func relayedPair(t *testing.T, ctx context.Context, upstream, model string) (server, client *Node) {
	t.Helper()
	boot := testNode(t, ctx, Config{})
	natted := func(cfg Config) *Node {
		cfg.Bootstrap, cfg.Private, cfg.DataDir = boot.Addrs()[:1], true, t.TempDir()
		cfg.AnnounceEvery, cfg.Concurrency = 300*time.Millisecond, 2
		n, err := New(ctx, cfg) // no Listen: unreachable except via relay
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.host.Close() })
		return n
	}
	server = natted(Config{Upstream: upstream, Models: map[string]string{model: "local"}})
	client = natted(Config{})
	return server, client
}

// limitedConn returns client's relayed connection to server, or nil.
func limitedConn(client, server *Node) network.Conn {
	for _, c := range client.host.Network().ConnsToPeer(server.host.ID()) {
		if strings.Contains(c.RemoteMultiaddr().String(), "/p2p-circuit") && c.Stat().Limited {
			return c
		}
	}
	return nil
}

// TestLimitedRelayCarriesInference: over a relay with circuit limits, a streamed
// answer and a plain one larger than the library's default data limit arrive
// whole, the receipt reaches the server, and the limits are the ones configured.
func TestLimitedRelayCarriesInference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	big := strings.Repeat("x", 1<<20)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local"}]}`)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":40,"completion_tokens":10}}`, big)
	}))
	defer up.Close()
	server, client := relayedPair(t, ctx, up.URL+"/v1", "m")

	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		w := post(client, `{"model":"m","stream":true,"messages":[]}`)
		if w.Code == 200 && strings.Contains(w.Body.String(), "[DONE]") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no streamed answer through the relay: %d %s", w.Code, w.Body)
		}
	}
	c := limitedConn(client, server)
	if c == nil {
		t.Fatalf("expected a limited relayed connection, have %v", client.host.Network().ConnsToPeer(server.host.ID()))
	}
	st := c.Stat()
	if st.Extra[circuitclient.StatLimitDuration] != relayCircuitDuration || st.Extra[circuitclient.StatLimitData] != uint64(relayCircuitData) {
		t.Fatalf("circuit limits = %v", st.Extra)
	}
	if w := post(client, `{"model":"m","messages":[]}`); w.Code != 200 || !strings.Contains(w.Body.String(), big) {
		t.Fatalf("plain answer through the relay: %d, %d bytes", w.Code, w.Body.Len())
	}
	if limitedConn(client, server) == nil {
		t.Fatal("the large answer did not stay on the relayed connection")
	}
}

// TestHelloOverLimitedRelay: hello works on a limited connection. A stream opened
// without the option, as hello did before v0.5.0, does not, and that costs only
// the speedup: the model still arrives by gossip.
func TestHelloOverLimitedRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local")
	defer up.Close()
	server, client := relayedPair(t, ctx, up.URL+"/v1", "m")

	for deadline := time.Now().Add(30 * time.Second); limitedConn(client, server) == nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no limited relayed connection")
		}
		client.addRelayPaths(server.host.ID())
		cctx, c := context.WithTimeout(ctx, 3*time.Second)
		client.host.Connect(cctx, peer.AddrInfo{ID: server.host.ID()})
		c()
	}
	for server.lastAnn.Load() == nil {
		time.Sleep(10 * time.Millisecond)
	}

	octx, ocancel := context.WithTimeout(ctx, time.Second)
	defer ocancel()
	if s, err := client.host.NewStream(octx, server.host.ID(), protocol.HelloProto); err == nil {
		s.Reset()
		t.Fatal("a stream without the option opened on a limited connection")
	}

	client.mu.Lock()
	delete(client.peers, server.host.ID())
	client.mu.Unlock()
	client.hello(ctx, server.host.ID())
	if a, ok := client.alive()[server.host.ID()]; !ok || len(a.Models) == 0 {
		t.Fatalf("hello over the relay: %+v, %v", a, ok)
	}
}
