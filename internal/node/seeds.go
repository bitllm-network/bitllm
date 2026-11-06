package node

import (
	"context"
	"log"
	"math/rand/v2"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// seedRefreshEvery is how often the seed list is resolved again, so that
	// changing a seed takes a DNS change and no restart.
	seedRefreshEvery = time.Hour
	// seedRefreshEmpty is how soon to look again with no seed at all.
	seedRefreshEmpty = time.Minute
)

// seeds is the current seed list, replaced whole and never changed in place, so
// a caller may keep reading it.
func (n *Node) seeds() []peer.AddrInfo {
	n.relayMu.RLock()
	defer n.relayMu.RUnlock()
	return n.relays
}

// isSeed reports whether p is in the seed list.
func (n *Node) isSeed(p peer.ID) bool {
	for _, ai := range n.seeds() {
		if ai.ID == p {
			return true
		}
	}
	return false
}

// resolveSeeds resolves each --bootstrap value on its own. A value that gives no
// peer keeps what it gave last in prev, so a name that fails to resolve loses
// nothing, even when another value on the list still answers.
func resolveSeeds(ctx context.Context, values []string, prev map[string][]peer.AddrInfo) (map[string][]peer.AddrInfo, []peer.AddrInfo, error) {
	byValue := make(map[string][]peer.AddrInfo, len(values))
	var all []peer.AddrInfo
	for _, v := range values {
		infos, err := ResolveBootstrap(ctx, []string{v})
		if err != nil {
			return nil, nil, err
		}
		if len(infos) == 0 {
			infos = prev[v]
		}
		byValue[v] = infos
		all = append(all, infos...)
	}
	return byValue, mergeAddrInfos(all), nil
}

// reseedLoop resolves the seed list again every hour or so. The jitter keeps
// nodes from asking DNS and dialing a new seed all at the same moment.
func (n *Node) reseedLoop(ctx context.Context) {
	for {
		wait := seedRefreshEvery + rand.N(seedRefreshEvery/6)
		if len(n.seeds()) == 0 {
			wait = seedRefreshEmpty
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		n.refreshSeeds(ctx)
	}
}

// refreshSeeds swaps in the seed list as DNS gives it now, unless that is empty.
// redialLoop keeps the seeds connected from then on; startSeed brings up a new
// one, and one that left the list stops being synced.
func (n *Node) refreshSeeds(ctx context.Context) {
	byValue, fresh, err := resolveSeeds(ctx, n.cfg.Bootstrap, n.seedValues)
	if err != nil {
		log.Printf("seed list: %v", err)
		return
	}
	fresh = withoutPeer(fresh, n.self)
	if len(fresh) == 0 {
		return
	}
	n.seedValues = byValue
	old := n.seeds()
	n.relayMu.Lock()
	n.relays = fresh
	n.relayMu.Unlock()

	inOld := map[peer.ID]bool{}
	for _, ai := range old {
		inOld[ai.ID] = true
	}
	for _, ai := range fresh {
		if !inOld[ai.ID] {
			log.Printf("seed list: new seed %s", ai.ID)
			n.startSeed(ctx, ai)
		}
		delete(inOld, ai.ID)
	}
	for id := range inOld {
		log.Printf("seed list: %s is no longer a seed", id)
		n.stopSeed(id)
	}
}

// startSeed connects to a seed and keeps syncing from it, in the background, so
// a seed that does not answer holds up nothing else. It does nothing if that
// seed is already running.
func (n *Node) startSeed(ctx context.Context, ai peer.AddrInfo) {
	n.relayMu.Lock()
	if _, running := n.seedStops[ai.ID]; running {
		n.relayMu.Unlock()
		return
	}
	sctx, stop := context.WithCancel(ctx)
	if n.seedStops == nil {
		n.seedStops = map[peer.ID]context.CancelFunc{}
	}
	n.seedStops[ai.ID] = stop
	n.relayMu.Unlock()
	go func() {
		c, cancel := context.WithTimeout(sctx, seedConnectTimeout)
		if err := n.host.Connect(c, ai); err != nil {
			log.Printf("bootstrap %s: %v", ai.ID, err)
		}
		cancel()
		n.syncLoop(sctx, ai.ID)
	}()
}

// stopSeed ends what startSeed began for a seed that left the list.
func (n *Node) stopSeed(id peer.ID) {
	n.relayMu.Lock()
	defer n.relayMu.Unlock()
	if stop, ok := n.seedStops[id]; ok {
		stop()
		delete(n.seedStops, id)
	}
}
