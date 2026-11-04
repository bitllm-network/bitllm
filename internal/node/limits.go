package node

import (
	"sync"
	"time"

	"github.com/bitllm-network/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Gossip limits. Peer ids cost nothing, so these do not stop a determined
// attacker alone; they keep gossip state from growing without limit.

const maxPeers = protocol.MaxPeers // announcing peers we remember

// originRate allows one gossip message per origin per interval.
type originRate struct {
	mu    sync.Mutex
	every time.Duration
	last  map[peer.ID]time.Time
}

func (o *originRate) allow(p peer.ID, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.last == nil {
		o.last = map[peer.ID]time.Time{}
	}
	if t, ok := o.last[p]; ok && now.Sub(t) < o.every {
		return false
	}
	if len(o.last) >= 4*maxPeers {
		for id, t := range o.last {
			if now.Sub(t) > o.every {
				delete(o.last, id)
			}
		}
	}
	o.last[p] = now
	return true
}
