// Package ledger is a node's private, bilateral record of work exchanged with
// each peer, kept for display. There is no global ledger, and nothing reads this
// one to decide whom to serve.
package ledger

import (
	"encoding/json"
	"log"
	"os"
	"sync"

	"github.com/bitllm-org/bitllm/internal/store"
	"github.com/libp2p/go-libp2p/core/peer"
)

// MaxPeers bounds the record, since peer ids are free. The peer with the least
// activity makes way for a new one.
const MaxPeers = 10000

// Ledger is the per-peer record.
type Ledger struct {
	mu       sync.Mutex
	peers    map[string]*Account
	save     *store.Debounced
	maxPeers int
}

// fileLedger is the on-disk shape, so the in-memory map can stay private.
type fileLedger struct {
	Peers map[string]*Account `json:"peers"`
}

// Account is what one peer gave us and took from us.
type Account struct {
	Gave int64 `json:"gave"` // credits of work this peer did for us
	Took int64 `json:"took"` // credits of work we did for this peer
}

// Open loads the ledger, creating it on first write. A file that does not parse
// is logged and treated as empty.
func Open(path string) *Ledger {
	l := &Ledger{peers: map[string]*Account{}, maxPeers: MaxPeers}
	l.save = store.NewDebounced(path, 0o600, 0, l.snapshot)
	b, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	var f fileLedger
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("ledger file %s: %v (starting empty)", path, err)
		return l
	}
	for k, a := range f.Peers {
		if a != nil {
			l.peers[k] = a
		}
	}
	for len(l.peers) > l.maxPeers {
		k, _ := l.leastActiveLocked()
		delete(l.peers, k)
		l.save.Mark()
	}
	return l
}

func (l *Ledger) snapshot() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return json.MarshalIndent(fileLedger{l.peers}, "", "  ")
}

// Flush writes pending changes now.
func (l *Ledger) Flush() error { return l.save.Flush() }

// Close flushes and stops scheduling writes.
func (l *Ledger) Close() error { return l.save.Close() }

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func (a *Account) activity() int64 { return abs(a.Gave) + abs(a.Took) }

func (l *Ledger) leastActiveLocked() (string, int64) {
	least, smallest := "", int64(-1)
	for k, a := range l.peers {
		if n := a.activity(); smallest < 0 || n < smallest {
			least, smallest = k, n
		}
	}
	return least, smallest
}

// Balance is how much p is ahead: positive means p has done more for us than we for p.
func (l *Ledger) Balance(p peer.ID) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.peers[p.String()]; a != nil {
		return a.Gave - a.Took
	}
	return 0
}

// Served records credits we did for p.
func (l *Ledger) Served(p peer.ID, credits int64) { l.add(p, 0, credits) }

// Received records credits p did for us.
func (l *Ledger) Received(p peer.ID, credits int64) { l.add(p, credits, 0) }

func (l *Ledger) add(p peer.ID, gave, took int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.peers[p.String()]
	if a == nil {
		if len(l.peers) >= l.maxPeers {
			// A newcomer with less activity than everyone here is the one left out.
			least, smallest := l.leastActiveLocked()
			if abs(gave)+abs(took) < smallest {
				return
			}
			delete(l.peers, least)
		}
		a = &Account{}
		l.peers[p.String()] = a
	}
	a.Gave += gave
	a.Took += took
	l.save.Mark()
}

// Snapshot returns a copy of every peer account, for the dashboard.
func (l *Ledger) Snapshot() map[string]Account {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Account, len(l.peers))
	for k, v := range l.peers {
		out[k] = *v
	}
	return out
}
