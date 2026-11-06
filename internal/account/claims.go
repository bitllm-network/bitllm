package account

import (
	"container/list"
	"encoding/json"
	"log"
	"os"
	"sort"
	"sync"

	"github.com/bitllm-org/bitllm/internal/store"
)

// Bounds on the claim store. Node ids and accounts are both free, so a claim
// from an account that is not a member earns no place that a member's claim
// could need. It still has to be stored, because it can arrive before the
// invite that makes its owner a member.
const (
	// MaxClaims bounds the whole store.
	MaxClaims = 20000
	// MaxPendingClaims bounds the claims whose owner is not a member. The oldest
	// goes first; a live node gossips its claim every 5 min, so it comes back.
	MaxPendingClaims = 2000
	// MaxOwnerClaims bounds one member's nodes. The oldest goes first.
	MaxOwnerClaims = 64
)

// claimEntry is one stored claim and its place in the eviction order.
type claimEntry struct {
	c       Claim
	seq     uint64
	owned   bool          // in byOwner, rather than pending
	pending *list.Element // set while in pending
}

// Claims keeps the newest valid claim per node. Each claim is in one of three
// places: this node's own, which is never evicted; byOwner, for an owner who is
// a member; pending, for everyone else.
type Claims struct {
	mu       sync.Mutex
	byNode   map[string]*claimEntry
	byOwner  map[string][]*claimEntry // oldest first
	pending  *list.List               // of *claimEntry, oldest first
	self     string
	isMember func(owner string) bool
	gen      uint64 // counts membership re-evaluations
	seq      uint64
	save     *store.Debounced

	maxTotal, maxPending, maxOwner int
}

// OpenClaims loads the claims file, dropping lines that no longer verify. A file
// that does not parse is logged and treated as empty.
func OpenClaims(path string) *Claims {
	cs := &Claims{byNode: map[string]*claimEntry{}, byOwner: map[string][]*claimEntry{}, pending: list.New(),
		maxTotal: MaxClaims, maxPending: MaxPendingClaims, maxOwner: MaxOwnerClaims}
	cs.save = store.NewDebounced(path, 0o600, 0, cs.snapshot)
	b, err := os.ReadFile(path)
	if err != nil {
		return cs
	}
	var m map[string]Claim
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("claims file %s: %v (starting empty)", path, err)
		return cs
	}
	loaded := make([]Claim, 0, len(m))
	for k, v := range m {
		if v.Node == k {
			loaded = append(loaded, v)
		}
	}
	ok := make([]bool, len(loaded))
	store.ForEach(len(loaded), func(i int) { ok[i] = loaded[i].Valid() })
	// The file does not record arrival order. Signed time is the closest thing.
	idx := make([]int, 0, len(loaded))
	for i := range loaded {
		if ok[i] {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		ca, cb := &loaded[idx[a]], &loaded[idx[b]]
		if ca.Time != cb.Time {
			return ca.Time < cb.Time
		}
		return ca.Node < cb.Node
	})
	for _, i := range idx {
		cs.insertLocked(loaded[i], false)
	}
	return cs
}

func (cs *Claims) snapshot() ([]byte, error) {
	cs.mu.Lock()
	out := make(map[string]Claim, len(cs.byNode))
	for k, e := range cs.byNode {
		out[k] = e.c
	}
	cs.mu.Unlock()
	return json.MarshalIndent(out, "", "  ")
}

// SetSelf names this node, whose claim is never evicted and reaches the disk at
// once.
func (cs *Claims) SetSelf(node string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.self = node
	if e := cs.byNode[node]; e != nil {
		cs.unlinkLocked(e)
	}
}

// SetMembership gives the store a way to ask whether an account is a member, and
// sorts what it already holds. The node sets it once both stores exist. Without
// it there is no membership to prefer, so all claims share MaxClaims.
//
// The store calls isMember with no lock held. Computing the member set reads
// this store, so holding the lock across that call would deadlock.
func (cs *Claims) SetMembership(isMember func(owner string) bool) {
	cs.mu.Lock()
	cs.isMember = isMember
	cs.mu.Unlock()
	cs.Reevaluate()
}

// memberOf asks about owner with no lock held, and returns locked. The answer is
// asked again if a re-evaluation ran meanwhile, so a claim cannot slip into the
// store with an answer older than the last membership change.
func (cs *Claims) memberOf(owner string) bool {
	for {
		cs.mu.Lock()
		gen, ask := cs.gen, cs.isMember
		cs.mu.Unlock()
		member := ask != nil && ask(owner)
		cs.mu.Lock()
		if cs.gen == gen {
			return member
		}
		cs.mu.Unlock()
	}
}

// Add stores c if it is valid and newer than what we have; it reports whether it did.
func (cs *Claims) Add(c Claim) bool {
	if !cs.newer(c) || !c.Valid() {
		return false
	}
	member := cs.memberOf(c.Owner)
	stored := cs.addLocked(c, member, true)
	own := stored && c.Node == cs.self
	cs.mu.Unlock()
	if own {
		if err := cs.save.Flush(); err != nil {
			log.Printf("save claims: %v", err)
		}
	}
	return stored
}

// AddBatch stores a batch from one sync and returns how many it kept. Nothing is
// evicted to make room below MaxClaims: the batch usually comes before the
// invites that make its owners members, so the caller applies those and then
// calls Reevaluate, which sorts the batch and enforces the bounds.
func (cs *Claims) AddBatch(batch []Claim) int {
	fresh := make([]Claim, 0, len(batch))
	for _, c := range batch {
		if cs.newer(c) {
			fresh = append(fresh, c)
		}
	}
	ok := make([]bool, len(fresh))
	store.ForEach(len(fresh), func(i int) { ok[i] = fresh[i].Valid() })
	stored := 0
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, c := range fresh {
		if ok[i] && cs.addLocked(c, false, false) {
			stored++
		}
	}
	return stored
}

// newer reports whether c could replace what we hold. It costs no signature
// check, so a claim we already have costs nothing to hear again.
func (cs *Claims) newer(c Claim) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	old := cs.byNode[c.Node]
	return old == nil || old.c.Time < c.Time
}

func (cs *Claims) addLocked(c Claim, member, trim bool) bool {
	old := cs.byNode[c.Node]
	if old != nil && old.c.Time >= c.Time {
		return false
	}
	if old == nil && c.Node != cs.self && !cs.roomLocked(member, trim) {
		return false
	}
	if old != nil {
		cs.removeLocked(old)
	}
	cs.insertLocked(c, member)
	if trim {
		cs.trimLocked()
	}
	cs.save.Mark()
	return true
}

// roomLocked says whether a new claim may enter a full store. A member's claim
// or this node's may push out others; a claim from anyone else may only replace
// another like it.
func (cs *Claims) roomLocked(member, trim bool) bool {
	if len(cs.byNode) < cs.maxTotal {
		return true
	}
	if cs.pending.Len() > 0 {
		cs.removeLocked(cs.pending.Front().Value.(*claimEntry))
		return true
	}
	if member && trim {
		cs.evictOldestOwnedLocked()
		return true
	}
	return false
}

func (cs *Claims) insertLocked(c Claim, member bool) {
	cs.seq++
	e := &claimEntry{c: c, seq: cs.seq}
	cs.byNode[c.Node] = e
	cs.placeLocked(e, member)
}

// placeLocked files an entry that is in no list yet.
func (cs *Claims) placeLocked(e *claimEntry, member bool) {
	switch {
	case e.c.Node == cs.self && cs.self != "":
	case member:
		e.owned = true
		cs.byOwner[e.c.Owner] = append(cs.byOwner[e.c.Owner], e)
	default:
		e.pending = cs.pending.PushBack(e)
	}
}

// unlinkLocked takes an entry out of its list, leaving it in byNode.
func (cs *Claims) unlinkLocked(e *claimEntry) {
	switch {
	case e.pending != nil:
		cs.pending.Remove(e.pending)
		e.pending = nil
	case e.owned:
		nodes := cs.byOwner[e.c.Owner]
		for i, x := range nodes {
			if x == e {
				nodes = append(nodes[:i:i], nodes[i+1:]...)
				break
			}
		}
		if len(nodes) == 0 {
			delete(cs.byOwner, e.c.Owner)
		} else {
			cs.byOwner[e.c.Owner] = nodes
		}
		e.owned = false
	}
}

func (cs *Claims) removeLocked(e *claimEntry) {
	cs.unlinkLocked(e)
	delete(cs.byNode, e.c.Node)
	cs.save.Mark()
}

// trimLocked enforces every bound: each member's cap, then the pending queue,
// then the total, taking pending claims before any member's.
func (cs *Claims) trimLocked() {
	for owner, nodes := range cs.byOwner {
		if over := len(nodes) - cs.maxOwner; over > 0 {
			for _, e := range nodes[:over:over] {
				e.owned = false
				delete(cs.byNode, e.c.Node)
			}
			cs.byOwner[owner] = nodes[over:]
			cs.save.Mark()
		}
	}
	limit := cs.maxPending
	if cs.isMember == nil {
		limit = cs.maxTotal
	}
	for cs.pending.Len() > limit {
		cs.removeLocked(cs.pending.Front().Value.(*claimEntry))
	}
	for len(cs.byNode) > cs.maxTotal {
		if cs.pending.Len() > 0 {
			cs.removeLocked(cs.pending.Front().Value.(*claimEntry))
		} else if !cs.evictOldestOwnedLocked() {
			return
		}
	}
}

// evictOldestOwnedLocked removes the oldest member claim. Only members can get
// here, and only by holding MaxOwnerClaims nodes each across many accounts.
func (cs *Claims) evictOldestOwnedLocked() bool {
	var oldest *claimEntry
	for _, nodes := range cs.byOwner {
		if oldest == nil || nodes[0].seq < oldest.seq {
			oldest = nodes[0]
		}
	}
	if oldest == nil {
		return false
	}
	cs.removeLocked(oldest)
	return true
}

// Reevaluate files each claim by whether its owner is a member now, then enforces
// the bounds. The node calls it after a membership change, so a claim that came
// before its invite moves to the member side.
func (cs *Claims) Reevaluate() {
	cs.mu.Lock()
	ask := cs.isMember
	if ask == nil {
		cs.trimLocked()
		cs.mu.Unlock()
		return
	}
	cs.gen++
	owners := make(map[string]bool, len(cs.byOwner))
	for _, e := range cs.byNode {
		owners[e.c.Owner] = false
	}
	cs.mu.Unlock()

	for o := range owners { // no lock held: see SetMembership
		owners[o] = ask(o)
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()
	entries := make([]*claimEntry, 0, len(cs.byNode))
	for _, e := range cs.byNode {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].seq < entries[b].seq })
	for _, e := range entries {
		member, known := owners[e.c.Owner]
		if !known || e.owned == member || e.c.Node == cs.self {
			continue
		}
		cs.unlinkLocked(e)
		cs.placeLocked(e, member)
	}
	for _, nodes := range cs.byOwner {
		sort.Slice(nodes, func(a, b int) bool { return nodes[a].seq < nodes[b].seq })
	}
	cs.trimLocked()
}

// Flush writes pending changes now.
func (cs *Claims) Flush() error { return cs.save.Flush() }

// Close flushes and stops scheduling writes.
func (cs *Claims) Close() error { return cs.save.Close() }

// Get returns the claim for a node, if any.
func (cs *Claims) Get(node string) (Claim, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if e := cs.byNode[node]; e != nil {
		return e.c, true
	}
	return Claim{}, false
}

// All returns a copy of every claim, for sync and the dashboard.
func (cs *Claims) All() []Claim {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]Claim, 0, len(cs.byNode))
	for _, e := range cs.byNode {
		out = append(out, e.c)
	}
	return out
}
