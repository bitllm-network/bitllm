package account_test

import (
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type owner struct {
	k    *secp256k1.PrivateKey
	addr string
}

func newOwner(t *testing.T) owner {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return owner{k, account.EthAddress(k.PubKey())}
}

// claim makes a fully signed claim of a new node, or of the given key.
func (o owner) claim(t *testing.T, at int64, keys ...crypto.PrivKey) account.Claim {
	t.Helper()
	var nk crypto.PrivKey
	if len(keys) > 0 {
		nk = keys[0]
	} else {
		nk, _, _ = crypto.GenerateEd25519Key(nil)
	}
	id, _ := peer.IDFromPrivateKey(nk)
	c, err := account.NewClaim(id, o.addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Time = at
	c.OwnerSig = hex.EncodeToString(account.EthSign(o.k, c.Message()))
	if err := account.SignClaimAsNode(c, nk); err != nil {
		t.Fatal(err)
	}
	return *c
}

func members(addrs ...string) func(string) bool {
	set := map[string]bool{}
	for _, a := range addrs {
		set[a] = true
	}
	return func(o string) bool { return set[o] }
}

func holds(cs *account.Claims, c account.Claim) bool {
	_, ok := cs.Get(c.Node)
	return ok
}

func mustAdd(t *testing.T, cs *account.Claims, c account.Claim) {
	t.Helper()
	if !cs.Add(c) {
		t.Fatal("claim refused")
	}
}

func TestPendingClaimsAreFirstInFirstOut(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(100, 3, 64)
	cs.SetMembership(members())
	var added []account.Claim
	for i := range 5 {
		c := newOwner(t).claim(t, int64(100-i)) // arrival decides, not the signed time
		mustAdd(t, cs, c)
		added = append(added, c)
	}
	if holds(cs, added[0]) || holds(cs, added[1]) {
		t.Fatal("the oldest arrivals were kept")
	}
	for _, c := range added[2:] {
		if !holds(cs, c) {
			t.Fatal("a newer arrival was evicted")
		}
	}
}

func TestStrangersCannotEvictAMember(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(6, 2, 64)
	m := newOwner(t)
	cs.SetMembership(members(m.addr))
	mine := m.claim(t, 1)
	mustAdd(t, cs, mine)
	for i := range 20 {
		cs.Add(newOwner(t).claim(t, int64(i)))
		if n := len(cs.All()); n > 6 {
			t.Fatalf("%d claims, over the total", n)
		}
	}
	if !holds(cs, mine) {
		t.Fatal("a stranger's claim evicted a member's")
	}
	// A newer claim from a stranger has a place; the member's is still there.
	late := newOwner(t).claim(t, 500)
	mustAdd(t, cs, late)
	if !holds(cs, late) || !holds(cs, mine) {
		t.Fatal("late stranger or member lost")
	}
}

func TestOwnClaimIsAlwaysKept(t *testing.T) {
	path := t.TempDir() + "/c.json"
	cs := account.OpenClaims(path)
	cs.SetLimits(3, 2, 2)
	m := newOwner(t)
	cs.SetMembership(members(m.addr))
	nk, _, _ := crypto.GenerateEd25519Key(nil)
	id, _ := peer.IDFromPrivateKey(nk)
	cs.SetSelf(id.String())

	// The store is already full of member claims when this node links.
	full := []account.Claim{newOwner(t).claim(t, 1), m.claim(t, 2), m.claim(t, 3)}
	for _, c := range full {
		cs.Add(c)
	}
	own := newOwner(t).claim(t, 10, nk) // its owner is not even a member
	if !cs.Add(own) {
		t.Fatal("this node could not link")
	}
	// It reached the disk at once: the CLI exits right after linking.
	if !holds(account.OpenClaims(path), own) {
		t.Fatal("own claim was not written at once")
	}
	for range 10 {
		cs.Add(newOwner(t).claim(t, 20))
		cs.Add(m.claim(t, 21))
		if !holds(cs, own) {
			t.Fatal("own claim evicted")
		}
		if n := len(cs.All()); n > 3 {
			t.Fatalf("%d claims, over the total", n)
		}
	}
	// Moving between sides does not touch it either.
	cs.SetMembership(members())
	if !holds(cs, own) {
		t.Fatal("own claim lost on re-evaluation")
	}
}

func TestMemberOwnerCapEvictsOldest(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(100, 10, 2)
	m := newOwner(t)
	cs.SetMembership(members(m.addr))
	a, b, c := m.claim(t, 1), m.claim(t, 2), m.claim(t, 3)
	mustAdd(t, cs, a)
	mustAdd(t, cs, b)
	mustAdd(t, cs, c)
	if holds(cs, a) || !holds(cs, b) || !holds(cs, c) {
		t.Fatal("cap did not evict the oldest")
	}
}

func TestDefaultLimits(t *testing.T) {
	if account.MaxClaims != 20000 || account.MaxPendingClaims != 2000 || account.MaxOwnerClaims != 64 {
		t.Fatal("limits changed")
	}
}

func TestFullStoreEvictsOldestMemberForAMember(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(2, 1, 64)
	a, b, c := newOwner(t), newOwner(t), newOwner(t)
	cs.SetMembership(members(a.addr, b.addr, c.addr))
	first, second, third := a.claim(t, 1), b.claim(t, 2), c.claim(t, 3)
	mustAdd(t, cs, first)
	mustAdd(t, cs, second)
	mustAdd(t, cs, third)
	if holds(cs, first) || !holds(cs, second) || !holds(cs, third) {
		t.Fatal("the oldest member claim should make way")
	}
	// A stranger gets no room from members.
	if cs.Add(newOwner(t).claim(t, 4)) {
		t.Fatal("a stranger took a member's place")
	}
}

func TestClaimMovesWhenItsOwnerJoinsOrLeaves(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(100, 1, 64)
	guest := newOwner(t)
	var isMember atomic.Bool
	cs.SetMembership(func(o string) bool { return o == guest.addr && isMember.Load() })

	early := guest.claim(t, 1) // arrives before the invite
	mustAdd(t, cs, early)
	isMember.Store(true)
	cs.Reevaluate()
	for range 5 {
		mustAdd(t, cs, newOwner(t).claim(t, 2))
	}
	if !holds(cs, early) {
		t.Fatal("a claim that came before its invite did not move")
	}

	// Revoked: it is the oldest arrival among strangers again only by position,
	// so the next stranger pushes it out.
	isMember.Store(false)
	cs.Reevaluate()
	mustAdd(t, cs, newOwner(t).claim(t, 3))
	if holds(cs, early) {
		t.Fatal("a former member kept the protected place")
	}
}

// The predicate reads the store, as computing the member set does.
func TestPredicateRunsWithoutTheLock(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetMembership(func(o string) bool {
		cs.Get(o)
		cs.All()
		return true
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		cs.Add(newOwner(t).claim(t, 1))
		cs.Reevaluate()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: the predicate ran under the store's lock")
	}
}

// Replaying invites at startup reads who has served, so nothing may be evicted
// before the store knows who the members are.
func TestLoadKeepsEverythingUntilSorted(t *testing.T) {
	path := t.TempDir() + "/c.json"
	cs := account.OpenClaims(path)
	var all []account.Claim
	m := newOwner(t)
	for i := range 6 {
		o := newOwner(t)
		if i == 0 {
			o = m
		}
		c := o.claim(t, int64(i))
		mustAdd(t, cs, c)
		all = append(all, c)
	}
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	again := account.OpenClaims(path)
	again.SetLimits(100, 2, 64)
	if len(again.All()) != 6 {
		t.Fatalf("%d claims before sorting", len(again.All()))
	}
	again.SetMembership(members(m.addr))
	if n := len(again.All()); n != 3 {
		t.Fatalf("%d claims after sorting, want the member and 2 others", n)
	}
	if !holds(again, all[0]) || !holds(again, all[5]) || holds(again, all[1]) {
		t.Fatal("wrong claims kept")
	}
}

func TestAddBatchWaitsForReevaluate(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(100, 1, 64)
	guest := newOwner(t)
	var isMember atomic.Bool
	cs.SetMembership(func(o string) bool { return o == guest.addr && isMember.Load() })
	early := guest.claim(t, 1)
	batch := []account.Claim{early, newOwner(t).claim(t, 2), newOwner(t).claim(t, 3)}
	if got := cs.AddBatch(batch); got != 3 {
		t.Fatalf("stored %d of 3", got)
	}
	if !holds(cs, early) {
		t.Fatal("a batch evicted its own claim before the invites applied")
	}
	isMember.Store(true)
	cs.Reevaluate()
	if !holds(cs, early) || len(cs.All()) != 2 {
		t.Fatalf("after sorting: %d claims", len(cs.All()))
	}
	if got := cs.AddBatch(batch[:1]); got != 0 {
		t.Fatalf("stored %d again", got)
	}
	bad := newOwner(t).claim(t, 9)
	bad.NodeSig = []byte("nope")
	if cs.AddBatch([]account.Claim{bad}) != 0 {
		t.Fatal("a batch stored a bad claim")
	}
}

func TestBatchHoldsTheTotal(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	cs.SetLimits(3, 2, 64)
	cs.SetMembership(members())
	var batch []account.Claim
	for i := range 6 {
		batch = append(batch, newOwner(t).claim(t, int64(i)))
	}
	cs.AddBatch(batch)
	if n := len(cs.All()); n != 3 {
		t.Fatalf("%d claims, over the total", n)
	}
	cs.Reevaluate()
	if n := len(cs.All()); n != 2 {
		t.Fatalf("%d claims after sorting", n)
	}
}

func TestBadFilesDoNotStopTheNode(t *testing.T) {
	m := newOwner(t)
	good := m.claim(t, 1)
	forged := good
	forged.Name = "forged"
	for name, content := range map[string]string{
		"not json": "not json",
		"null":     "null",
		"forged":   fmt.Sprintf(`{%q: {"node": %q, "owner": %q, "name":"forged", "time": 1}}`, good.Node, good.Node, good.Owner),
		"misfiled": fmt.Sprintf(`{"other": {"node": %q, "owner": %q, "time": 1}}`, good.Node, good.Owner),
	} {
		path := t.TempDir() + "/c.json"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cs := account.OpenClaims(path)
		if len(cs.All()) != 0 {
			t.Fatalf("%s: loaded %d claims", name, len(cs.All()))
		}
		mustAdd(t, cs, good)
	}
}

func TestReplacingAClaimKeepsOneEntry(t *testing.T) {
	cs := account.OpenClaims(t.TempDir() + "/c.json")
	a, b := newOwner(t), newOwner(t)
	cs.SetMembership(members(a.addr))
	nk, _, _ := crypto.GenerateEd25519Key(nil)
	mustAdd(t, cs, a.claim(t, 1, nk))
	moved := b.claim(t, 2, nk)
	mustAdd(t, cs, moved)
	if got, ok := cs.Get(moved.Node); !ok || got.Owner != b.addr || len(cs.All()) != 1 {
		t.Fatalf("after replace: %+v %v, %d claims", got, ok, len(cs.All()))
	}
	if cs.Add(a.claim(t, 2, nk)) {
		t.Fatal("a claim no newer than the current one replaced it")
	}
}
