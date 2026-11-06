package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// announcedSlots fits --concurrency into the range an announce may state. The
// serving queue keeps using the configured number.
func announcedSlots(concurrency, inflight int) (free, slots int, clamped bool) {
	slots = min(max(concurrency, 1), protocol.MaxAnnounceSlots)
	free = min(max(concurrency-inflight, 0), slots)
	return free, slots, slots != concurrency
}

// announced returns the models this node gossips right now: none while its
// server is down.
func (n *Node) announced() []string {
	if a := n.lastAnn.Load(); a != nil {
		return a.Models
	}
	return nil
}

// announceProblems is what kept the last announce from saying everything.
type announceProblems struct {
	omitted int    // model names left out
	clamped bool   // --concurrency does not fit an announce
	publish string // error from the last publish
}

// String is the announce_error value in GET /status; empty when all is well.
func (p announceProblems) String() string {
	var parts []string
	if p.omitted > 0 {
		parts = append(parts, fmt.Sprintf("%d models not announced", p.omitted))
	}
	if p.clamped {
		parts = append(parts, fmt.Sprintf("--concurrency announced within 1 to %d", protocol.MaxAnnounceSlots))
	}
	if p.publish != "" {
		parts = append(parts, "publish failed: "+p.publish)
	}
	return strings.Join(parts, "; ")
}

// announceLog logs each problem once, not once per announce period.
type announceLog struct {
	omitted   int
	clamped   bool
	published map[string]bool
}

// maxPublishErrors bounds the publish errors remembered as already logged.
const maxPublishErrors = 16

func (l *announceLog) report(p announceProblems, slots int) {
	if p.omitted != l.omitted {
		log.Printf("announce: %d models left out (invalid names, or more than %d); canonical names go first; pick which with --model", p.omitted, protocol.MaxAnnounceModels)
		l.omitted = p.omitted
	}
	if p.clamped && !l.clamped {
		log.Printf("announce: --concurrency is outside 1 to %d, announcing %d slots", protocol.MaxAnnounceSlots, slots)
	}
	l.clamped = p.clamped
	if p.publish != "" && !l.published[p.publish] && len(l.published) < maxPublishErrors {
		if l.published == nil {
			l.published = map[string]bool{}
		}
		l.published[p.publish] = true
		log.Printf("announce: publish failed: %s", p.publish)
	}
}

// maxRelayAddrs caps the addresses kept for one announced relay.
const maxRelayAddrs = 8

// relayAddrs picks the addresses of an announced relay that count: IP addresses
// a NATed node can dial itself, public ones only, at most maxRelayAddrs. A relay
// reached through another relay cannot hold a reservation. On a network whose
// seeds all have private addresses (local is true), private addresses count too.
func relayAddrs(in []ma.Multiaddr, local bool) []ma.Multiaddr {
	var out []ma.Multiaddr
	for _, a := range in {
		if len(out) == maxRelayAddrs {
			break
		}
		if _, err := a.ValueForProtocol(ma.P_CIRCUIT); err == nil {
			continue
		}
		_, err4 := a.ValueForProtocol(ma.P_IP4)
		_, err6 := a.ValueForProtocol(ma.P_IP6)
		if (err4 == nil || err6 == nil) && (local || manet.IsPublicAddr(a)) {
			out = append(out, a)
		}
	}
	return out
}

// onLocalNetwork reports whether every seed we were given has a private address,
// as on a LAN or in a test. The public network always has a public seed.
func (n *Node) onLocalNetwork() bool {
	if len(n.relays) == 0 {
		return false
	}
	for _, ai := range n.relays {
		for _, a := range ai.Addrs {
			if manet.IsPublicAddr(a) {
				return false
			}
		}
	}
	return true
}

const (
	relayLookupPerPeer = 10 * time.Minute // at most once per peer
	relayLookupGap     = 5 * time.Second  // and at most one per this long, whoever it is for
	relayLookupLimit   = 15 * time.Second // longest one lookup may run
)

// relayLookups is the state behind lookupRelay.
type relayLookups struct {
	mu    sync.Mutex
	busy  bool
	last  time.Time
	tried map[peer.ID]time.Time
}

// allow reports whether a lookup of p may start now, and if so marks it started.
func (l *relayLookups) allow(p peer.ID, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy || now.Sub(l.last) < relayLookupGap {
		return false
	}
	if t, ok := l.tried[p]; ok && now.Sub(t) < relayLookupPerPeer {
		return false
	}
	if l.tried == nil {
		l.tried = map[peer.ID]time.Time{}
	}
	if len(l.tried) >= maxPeers {
		for id, t := range l.tried {
			if now.Sub(t) >= relayLookupPerPeer {
				delete(l.tried, id)
			}
		}
		if len(l.tried) >= maxPeers {
			return false
		}
	}
	l.busy, l.last, l.tried[p] = true, now, now
	return true
}

func (l *relayLookups) done() {
	l.mu.Lock()
	l.busy = false
	l.mu.Unlock()
}

// lookupRelay asks the DHT where an announced relay can be dialed, in the
// background: one lookup at a time, limited as above.
func (n *Node) lookupRelay(p peer.ID) {
	if n.dht == nil || !n.relayFinds.allow(p, time.Now()) {
		return
	}
	go func() {
		defer n.relayFinds.done()
		ctx, cancel := context.WithTimeout(n.ctx, relayLookupLimit)
		defer cancel()
		ai, err := n.dht.FindPeer(ctx, p)
		if err != nil {
			return
		}
		addrs := relayAddrs(ai.Addrs, n.onLocalNetwork())
		n.mu.Lock()
		if pi, ok := n.peers[p]; ok && len(pi.addrs) == 0 {
			pi.addrs = addrs
		}
		n.mu.Unlock()
	}()
}
