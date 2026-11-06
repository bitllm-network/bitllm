package node

import (
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// How long a peer ranks behind the others after a failure that was not a busy
// refusal. These are vars so tests can shorten them.
var (
	peerCooldown    = 60 * time.Second
	peerCooldownMax = 15 * time.Minute
)

const (
	maxPeerMemory    = 1024          // entries kept, across peers and models
	answeredRecently = 1 * time.Hour // how long a complete answer counts in a peer's favour
)

type peerModel struct {
	id    peer.ID
	model string
}

type peerRecord struct {
	until   time.Time     // in cooldown until then
	backoff time.Duration // length of the last cooldown
	okAt    time.Time     // last complete answer
	touched time.Time     // last change, for eviction
}

// peerMemory is how each peer has done for each model, as seen from here. The
// zero value is ready to use.
type peerMemory struct {
	mu sync.Mutex
	m  map[peerModel]peerRecord
}

// put stores a record, evicting the one unchanged for longest when full.
func (pm *peerMemory) put(k peerModel, r peerRecord, now time.Time) {
	if pm.m == nil {
		pm.m = map[peerModel]peerRecord{}
	}
	if _, ok := pm.m[k]; !ok && len(pm.m) >= maxPeerMemory {
		var oldest peerModel
		at := now
		for o, or := range pm.m {
			if !or.touched.After(at) {
				oldest, at = o, or.touched
			}
		}
		delete(pm.m, oldest)
	}
	r.touched = now
	pm.m[k] = r
}

// failed starts a cooldown, twice as long as the last one. Failures while one
// is running, such as parallel requests hitting the same outage, count once.
func (pm *peerMemory) failed(p peer.ID, model string, now time.Time) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	k := peerModel{p, model}
	r := pm.m[k]
	if now.Before(r.until) {
		return
	}
	r.backoff = min(max(2*r.backoff, peerCooldown), peerCooldownMax)
	r.until = now.Add(r.backoff)
	pm.put(k, r, now)
}

// answered records a complete answer and ends any cooldown.
func (pm *peerMemory) answered(p peer.ID, model string, now time.Time) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.put(peerModel{p, model}, peerRecord{okAt: now}, now)
}

// rank says whether p is in cooldown for model, and whether it answered recently.
func (pm *peerMemory) rank(p peer.ID, model string, now time.Time) (cooling, recent bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	r := pm.m[peerModel{p, model}]
	return now.Before(r.until), !r.okAt.IsZero() && now.Sub(r.okAt) < answeredRecently
}
