// Package members is the invite tree: who may use the network, derived
// from signed invites and revocations, with no membership server.
package members

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/store"
)

// Membership is an invite tree of Ethereum accounts, private-tracker style.
// Roots are members; so is anyone validly invited by a member. Every node
// computes the same member set from the same signed invites and revocations,
// so there is no membership server. The rules:
//
//  1. Members are served on any free slot; uninvited nodes only when the
//     server is idle, unless it runs with --open.
//  2. No daily quota by default; an owner can set one (--free-daily).
//  3. An account that has ever served, by the public receipts, gets 10 invites,
//     not 3. Every node must reach the same member set, so this reads only what
//     all nodes can see. Who is exempt from a server's quota is that server's own
//     decision and is not part of this package.

const (
	// MemberInvites is how many accounts an ordinary member may invite.
	MemberInvites = 3
	// ContributorInvites is the higher limit for an account that has ever served.
	ContributorInvites = 10
	// ContributorWindow is how recently an account's node must have done a job
	// for this node for the account to count as a contributor here.
	ContributorWindow = 7 * 24 * time.Hour
	// MaxInvites bounds the stored invite set; ids are cheap, so cap it.
	MaxInvites = 50_000
)

// Invite says inviter (a member) vouches for invitee.
type Invite struct {
	Inviter string `json:"inviter"`
	Invitee string `json:"invitee"`
	Time    int64  `json:"time"`
	Sig     string `json:"sig"` // personal_sign by inviter of Message()
}

func (i *Invite) Message() string {
	return fmt.Sprintf("BitLLM invite\ninviter: %s\ninvitee: %s\ntime: %d", i.Inviter, i.Invitee, i.Time)
}

func (i *Invite) id() string { return i.Inviter + ">" + i.Invitee }

func signedBy(msg, sigHex, addr string) bool {
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil {
		return false
	}
	got, err := account.EthRecover(msg, sig)
	return err == nil && got == addr
}

func (i *Invite) Valid() bool {
	return account.ValidAddress(i.Inviter) && account.ValidAddress(i.Invitee) && i.Inviter != i.Invitee &&
		i.Time <= time.Now().Add(10*time.Minute).Unix() && signedBy(i.Message(), i.Sig, i.Inviter)
}

// Revoke is a root removing an account. It loses membership and so does
// everyone it invited, down the tree, unless someone else invited them too.
type Revoke struct {
	Account string `json:"account"`
	Root    string `json:"root"`
	Time    int64  `json:"time"`
	Sig     string `json:"sig"` // personal_sign by root of Message()
}

func (r *Revoke) Message() string {
	return fmt.Sprintf("BitLLM revoke\naccount: %s\nroot: %s\ntime: %d", r.Account, r.Root, r.Time)
}

func (r *Revoke) Valid() bool {
	return account.ValidAddress(r.Account) && account.ValidAddress(r.Root) &&
		r.Time <= time.Now().Add(10*time.Minute).Unix() && signedBy(r.Message(), r.Sig, r.Root)
}

// Item is one gossip or sync message on the membership topic.
type Item struct {
	Invite *Invite `json:"invite,omitempty"`
	Revoke *Revoke `json:"revoke,omitempty"`
}

// Members stores invites and revocations and derives the member set. It only
// stores (and so only forwards) an invite that counts at the time it arrives:
// from a member with invites left, for an account that is not a member yet.
// Signing keys are free, so storing every validly signed invite would let
// outsiders fill the store; this way it holds at most members x 10 invites.
type Members struct {
	adding  sync.Mutex // one writer at a time, across its checks; Compute does not take it
	mu      sync.Mutex
	roots   map[string]bool
	invites map[string]Invite // by id
	revokes map[string]Revoke // by account, which only a root can add
	// served reports which accounts have ever served a job. Set by the node;
	// nil means nobody has.
	served   func() map[string]bool
	save     *store.Debounced
	computes atomic.Int64
}

// Open loads the store. Saved items are replayed through the same
// checks as new ones (revocations first, then invites in time order), so a
// store written by an older or buggy version cannot carry junk forward. A file
// that does not parse is logged and treated as empty.
func Open(path string, roots []string, served func() map[string]bool) *Members {
	m := &Members{roots: map[string]bool{}, invites: map[string]Invite{}, revokes: map[string]Revoke{}, served: served}
	m.save = store.NewDebounced(path, 0o600, 0, m.snapshot)
	for _, r := range roots {
		m.roots[strings.ToLower(r)] = true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var items []Item
	if err := json.Unmarshal(b, &items); err != nil {
		log.Printf("members file %s: %v (starting empty)", path, err)
		return m
	}
	stored := 0
	for _, ok := range m.addBatch(items, false) {
		if ok {
			stored++
		}
	}
	if stored != len(items) { // junk was dropped; replaying what is left needs no write
		m.save.Mark()
	}
	return m
}

func (m *Members) snapshot() ([]byte, error) { return json.Marshal(m.Items()) }

// Flush writes pending changes now.
func (m *Members) Flush() error { return m.save.Flush() }

// Close flushes and stops scheduling writes.
func (m *Members) Close() error { return m.save.Close() }

// sortItems orders revocations first, then invites by time, which is the order
// in which they can be checked (an inviter's own invite before the ones it signs).
func sortItems(items []Item) {
	sort.SliceStable(items, func(a, b int) bool { return lessItem(items[a], items[b]) })
}

func lessItem(a, b Item) bool {
	key := func(it Item) (int, int64) {
		switch {
		case it.Revoke != nil:
			return 0, it.Revoke.Time
		case it.Invite != nil:
			return 1, it.Invite.Time
		}
		return 2, 0
	}
	ka, ta := key(a)
	kb, tb := key(b)
	switch {
	case ka != kb:
		return ka < kb
	case ta != tb:
		return ta < tb
	case a.Revoke != nil && b.Revoke != nil:
		return a.Revoke.Account < b.Revoke.Account
	case a.Invite != nil && b.Invite != nil:
		return a.Invite.id() < b.Invite.id()
	}
	return false
}

// Enabled reports whether membership is in force (some root is configured).
func (m *Members) Enabled() bool { return len(m.roots) > 0 }

// Check says why an item would not be stored, or nil if it would.
func (m *Members) Check(it Item) error {
	switch {
	case it.Invite != nil:
		i := it.Invite
		if !i.Valid() {
			return fmt.Errorf("invalid invite signature or addresses")
		}
		if !m.Enabled() {
			return nil
		}
		ms := m.Compute()
		switch lim := ms.InviteLimit(i.Inviter); {
		case !ms.Member[i.Inviter]:
			return fmt.Errorf("%s is not a member, so its invites do not count", account.ChecksumAddress(i.Inviter))
		case lim >= 0 && ms.Used[i.Inviter] >= lim:
			return fmt.Errorf("%s has used all %d invites", account.ChecksumAddress(i.Inviter), lim)
		case ms.Member[i.Invitee]:
			return fmt.Errorf("%s is already a member", account.ChecksumAddress(i.Invitee))
		case ms.revoked[i.Invitee]:
			return fmt.Errorf("%s was revoked by a root", account.ChecksumAddress(i.Invitee))
		}
		return nil
	case it.Revoke != nil:
		r := it.Revoke
		if !r.Valid() || !m.roots[r.Root] {
			return fmt.Errorf("only a root can revoke")
		}
		if m.roots[r.Account] {
			return fmt.Errorf("a root cannot be revoked")
		}
		return nil
	}
	return fmt.Errorf("empty item")
}

// Add stores an invite or revocation if it passes Check and is new; it
// reports whether it did.
func (m *Members) Add(it Item) bool {
	if m.Known(it) {
		return false
	}
	m.adding.Lock()
	defer m.adding.Unlock()
	if m.Check(it) != nil {
		return false
	}
	m.mu.Lock()
	stored := m.storeLocked(it)
	m.mu.Unlock()
	if stored {
		if it.Invite == nil {
			m.pruneInvites()
		}
		m.save.Mark()
	}
	return stored
}

func (m *Members) storeLocked(it Item) bool {
	switch {
	case it.Invite != nil:
		i := *it.Invite
		if _, ok := m.invites[i.id()]; ok || len(m.invites) >= MaxInvites {
			return false
		}
		m.invites[i.id()] = i
	case it.Revoke != nil:
		r := *it.Revoke
		if _, ok := m.revokes[r.Account]; ok {
			return false
		}
		m.revokes[r.Account] = r
	}
	return true
}

// pruneInvites drops every invite that no longer counts, as a restart would.
// Revoking a member removes its subtree, and the room those invites held is free
// again right away. The caller holds m.adding, so nothing is stored between the
// compute and the delete.
func (m *Members) pruneInvites() {
	m.mu.Lock()
	empty := len(m.invites) == 0
	m.mu.Unlock()
	if empty || !m.Enabled() {
		return
	}
	ms := m.Compute()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, i := range m.invites {
		if ms.InvitedBy[i.Invitee] != i.Inviter {
			delete(m.invites, id)
		}
	}
}

// AddBatch is Add for many items, as a sync delivers them: it applies them in
// replay order and reports for each item, by its position in items, whether it
// was stored. One Compute serves the whole batch in the usual case, so a batch of
// N is not N computes.
func (m *Members) AddBatch(items []Item) []bool { return m.addBatch(items, true) }

func (m *Members) addBatch(items []Item, mark bool) []bool {
	stored := make([]bool, len(items))
	order := make([]int, 0, len(items))
	for i, it := range items {
		if (it.Invite != nil || it.Revoke != nil) && !m.Known(it) {
			order = append(order, i)
		}
	}
	if len(order) == 0 {
		return stored
	}
	sort.SliceStable(order, func(a, b int) bool {
		return lessItem(items[order[a]], items[order[b]])
	})
	sigOK := make([]bool, len(items))
	store.ForEach(len(order), func(k int) {
		i := order[k]
		sigOK[i] = items[i].validSig()
	})

	m.adding.Lock()
	defer m.adding.Unlock()
	st := batchState{m: m}
	changed, revoked := false, false
	for _, i := range order {
		it := items[i]
		if !sigOK[i] {
			continue
		}
		if it.Invite != nil {
			if revoked { // room freed by the revocations is there for these invites
				m.pruneInvites()
				revoked = false
				st.ms = nil
			}
			if !st.applyInvite(it.Invite) {
				continue
			}
		} else {
			if !m.roots[it.Revoke.Root] || m.roots[it.Revoke.Account] {
				continue
			}
			m.mu.Lock()
			ok := m.storeLocked(it)
			m.mu.Unlock()
			if !ok {
				continue
			}
			revoked = true
		}
		stored[i], changed = true, true
	}
	if revoked {
		m.pruneInvites()
	}
	if changed && mark {
		m.save.Mark()
	}
	return stored
}

// validSig is the part of Check that needs no state.
func (it Item) validSig() bool {
	if it.Invite != nil {
		return it.Invite.Valid()
	}
	return it.Revoke.Valid()
}

// inviteKey is where an invite falls in Compute's order.
type inviteKey struct {
	time int64
	id   string
}

func (a inviteKey) before(b inviteKey) bool {
	if a.time != b.time {
		return a.time < b.time
	}
	return a.id < b.id
}

func keyOf(i *Invite) inviteKey { return inviteKey{i.Time, i.id()} }

// batchState is the member set during a batch, kept current without a Compute
// per item. An invite's effect can be worked out from the result of the last
// Compute unless it would change what a later invite did, and only then does the
// next item compute again.
type batchState struct {
	m        *Members
	ms       *MemberSet
	since    map[string]inviteKey // the invite that made each non-root a member
	inviters map[string]bool      // everyone with a stored invite to their name
}

// load computes the set if the last item invalidated it.
func (b *batchState) load() {
	if b.ms != nil {
		return
	}
	b.since = map[string]inviteKey{}
	b.ms = b.m.compute(b.since)
	b.m.mu.Lock()
	b.inviters = make(map[string]bool, len(b.m.invites))
	for _, i := range b.m.invites {
		b.inviters[i.Inviter] = true
	}
	b.m.mu.Unlock()
}

// applyInvite runs Check's rules on a signature-checked invite and stores it if
// it passes, leaving the state as Compute would leave it.
func (b *batchState) applyInvite(i *Invite) bool {
	m := b.m
	if !m.Enabled() {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.storeLocked(Item{Invite: i})
	}
	b.load()
	ms := b.ms
	if lim := ms.InviteLimit(i.Inviter); !ms.Member[i.Inviter] || (lim >= 0 && ms.Used[i.Inviter] >= lim) ||
		ms.Member[i.Invitee] || ms.revoked[i.Invitee] {
		return false
	}
	m.mu.Lock()
	ok := m.storeLocked(Item{Invite: i})
	m.mu.Unlock()
	if !ok {
		return false
	}
	switch since, invited := b.since[i.Inviter]; {
	case invited && keyOf(i).before(since):
		// Dated before its inviter was a member, so Compute skips it.
	case b.inviters[i.Invitee]:
		// The invitee has invites of its own stored that did not count until now.
		b.ms = nil
	default:
		// The inviter is under its limit, so its later invites all still count.
		// Nothing stored depends on the invitee being a member. So the invite
		// changes these entries only, wherever it falls in the order.
		ms.Used[i.Inviter]++
		ms.Member[i.Invitee] = true
		ms.InvitedBy[i.Invitee] = i.Inviter
		b.since[i.Invitee] = keyOf(i)
	}
	if b.ms != nil {
		b.inviters[i.Inviter] = true
	}
	return true
}

// Known reports whether exactly this item is already stored. Gossip checks
// a node's own messages too, and re-forwarding a known item is harmless.
func (m *Members) Known(it Item) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case it.Invite != nil:
		old, ok := m.invites[it.Invite.id()]
		return ok && old == *it.Invite
	case it.Revoke != nil:
		old, ok := m.revokes[it.Revoke.Account]
		return ok && old == *it.Revoke
	}
	return false
}

// Items returns everything stored, in replay order.
func (m *Members) Items() []Item {
	m.mu.Lock()
	out := make([]Item, 0, len(m.invites)+len(m.revokes))
	for _, i := range m.invites {
		out = append(out, Item{Invite: &i})
	}
	for _, r := range m.revokes {
		out = append(out, Item{Revoke: &r})
	}
	m.mu.Unlock()
	sortItems(out)
	return out
}

// MemberSet is the derived state at one point in time.
type MemberSet struct {
	Member    map[string]bool
	InvitedBy map[string]string
	Used      map[string]int  // invites used
	Sharers   map[string]bool // ever served a job, by the public receipts: 10 invites
	roots     map[string]bool
	revoked   map[string]bool
}

// InviteLimit is how many accounts a member may invite; -1 means no limit.
// Having served once is enough for 10, so the limit never drops later and
// invites already made never become invalid.
func (s *MemberSet) InviteLimit(a string) int {
	switch {
	case s.roots[a]:
		return -1
	case !s.Member[a]:
		return 0
	case s.Sharers[a]:
		return ContributorInvites
	}
	return MemberInvites
}

// IsRoot reports whether an account is a configured root.
func (s *MemberSet) IsRoot(a string) bool { return s.roots[a] }

// Compute derives the member set. Invites apply in time order; one counts
// only if its inviter was already a member and under its invite limit; the
// first such invite of an account wins. A revoked account counts as never
// invited and its invites count for nothing, so revoking an account also
// removes everyone it brought in (unless someone else invited them too).
func (m *Members) Compute() *MemberSet { return m.compute(nil) }

// compute also records in since, if given, the invite that made each non-root a
// member.
func (m *Members) compute(since map[string]inviteKey) *MemberSet {
	m.computes.Add(1)
	m.mu.Lock()
	invites := make([]Invite, 0, len(m.invites))
	for _, i := range m.invites {
		invites = append(invites, i)
	}
	revoked := map[string]bool{}
	for a := range m.revokes {
		if !m.roots[a] {
			revoked[a] = true
		}
	}
	roots, served := m.roots, m.served
	m.mu.Unlock()

	ever := map[string]bool{}
	if served != nil {
		ever = served()
	}
	sort.Slice(invites, func(a, b int) bool {
		if invites[a].Time != invites[b].Time {
			return invites[a].Time < invites[b].Time
		}
		return invites[a].id() < invites[b].id()
	})
	s := &MemberSet{Member: map[string]bool{}, InvitedBy: map[string]string{}, Used: map[string]int{},
		Sharers: ever, roots: roots, revoked: revoked}
	for r := range roots {
		s.Member[r] = true
	}
	for k := range invites {
		i := &invites[k]
		if s.Member[i.Invitee] || !s.Member[i.Inviter] || revoked[i.Inviter] || revoked[i.Invitee] {
			continue
		}
		if lim := s.InviteLimit(i.Inviter); lim >= 0 && s.Used[i.Inviter] >= lim {
			continue
		}
		s.Used[i.Inviter]++
		s.Member[i.Invitee] = true
		s.InvitedBy[i.Invitee] = i.Inviter
		if since != nil {
			since[i.Invitee] = keyOf(i)
		}
	}
	return s
}
