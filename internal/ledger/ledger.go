// Package ledger is a node's private, bilateral record of work exchanged
// with each peer. There is no global ledger: every node decides on its own
// whom to serve, preferring peers that have served it.
package ledger

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Ledger is the per-peer record, BitTorrent style.
type Ledger struct {
	mu    sync.Mutex
	path  string
	peers map[string]*Account
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

// Open loads the ledger, creating it on first write.
func Open(path string) *Ledger {
	l := &Ledger{path: path, peers: map[string]*Account{}}
	if b, err := os.ReadFile(path); err == nil {
		var f fileLedger
		if json.Unmarshal(b, &f) == nil && f.Peers != nil {
			l.peers = f.Peers
		}
	}
	return l
}

func (l *Ledger) acct(p peer.ID) *Account {
	a := l.peers[p.String()]
	if a == nil {
		a = &Account{}
		l.peers[p.String()] = a
	}
	return a
}

// Balance is how much p is ahead: positive means p has done more for us than we for p.
func (l *Ledger) Balance(p peer.ID) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.acct(p)
	return a.Gave - a.Took
}

// Served records credits we did for p.
func (l *Ledger) Served(p peer.ID, credits int64) { l.add(p, 0, credits) }

// Received records credits p did for us.
func (l *Ledger) Received(p peer.ID, credits int64) { l.add(p, credits, 0) }

func (l *Ledger) add(p peer.ID, gave, took int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.acct(p)
	a.Gave += gave
	a.Took += took
	b, _ := json.MarshalIndent(fileLedger{l.peers}, "", "  ")
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, l.path)
	}
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
