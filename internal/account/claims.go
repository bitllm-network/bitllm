package account

import (
	"encoding/json"
	"os"
	"sync"
)

// MaxClaims bounds the claim map. Node ids are free, so without it anyone
// could make it grow without limit.
const MaxClaims = 20000

// Claims keeps the newest valid claim per node.
type Claims struct {
	mu     sync.Mutex
	path   string
	byNode map[string]Claim
}

// OpenClaims loads the claims file, dropping lines that no longer verify.
func OpenClaims(path string) *Claims {
	c := &Claims{path: path, byNode: map[string]Claim{}}
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]Claim
		if json.Unmarshal(b, &m) == nil {
			for k, v := range m {
				if v.Valid() && v.Node == k {
					c.byNode[k] = v
				}
			}
		}
	}
	return c
}

// Add stores c if it is valid and newer than what we have; it reports whether it did.
func (cs *Claims) Add(c Claim) bool {
	if !c.Valid() {
		return false
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	old, ok := cs.byNode[c.Node]
	if ok && old.Time >= c.Time {
		return false
	}
	if !ok && len(cs.byNode) >= MaxClaims { // node ids are free; bound the map
		return false
	}
	cs.byNode[c.Node] = c
	b, _ := json.MarshalIndent(cs.byNode, "", "  ")
	tmp := cs.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, cs.path)
	}
	return true
}

// Get returns the claim for a node, if any.
func (cs *Claims) Get(node string) (Claim, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.byNode[node]
	return c, ok
}

// All returns a copy of every claim, for sync and the dashboard.
func (cs *Claims) All() []Claim {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]Claim, 0, len(cs.byNode))
	for _, c := range cs.byNode {
		out = append(out, c)
	}
	return out
}
