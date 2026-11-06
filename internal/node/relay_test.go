package node

import (
	"context"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
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
