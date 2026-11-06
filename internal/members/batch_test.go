package members_test

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/members"
	"github.com/bitllm-org/bitllm/internal/store"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// tree generates invites from root down, each account inviting up to fan others,
// with times in the order they were made. It uses all cores: signing is the slow
// part of the scale tests, and it is not what they measure.
func tree(t *testing.T, root ethKey, n, fan int) []members.Item {
	t.Helper()
	keys := make([]ethKey, n)
	store.ForEach(n, func(i int) {
		k, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			panic(err)
		}
		keys[i] = ethKey{k, account.EthAddress(k.PubKey())}
	})
	items := make([]members.Item, n)
	store.ForEach(n, func(i int) {
		from := root
		if i >= 1 {
			from = keys[(i-1)/fan]
		}
		items[i] = from.invite(keys[i].addr, int64(1000+i))
	})
	return items
}

func summary(ms *members.MemberSet) (map[string]bool, map[string]string, map[string]int) {
	return ms.Member, ms.InvitedBy, ms.Used
}

func sameSet(t *testing.T, what string, a, b *members.MemberSet) {
	t.Helper()
	am, ai, au := summary(a)
	bm, bi, bu := summary(b)
	if !reflect.DeepEqual(am, bm) || !reflect.DeepEqual(ai, bi) || !reflect.DeepEqual(au, bu) {
		t.Fatalf("%s: member sets differ: %d vs %d members", what, len(am), len(bm))
	}
}

func itemIDs(items []members.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		b, _ := json.Marshal(it)
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

func TestAddBatchUsesOneCompute(t *testing.T) {
	root := newEthKey(t)
	items := tree(t, root, 2000, members.MemberInvites)
	m := members.Open(t.TempDir()+"/m.json", []string{root.addr}, nil)
	stored := m.AddBatch(items)
	for i, ok := range stored {
		if !ok {
			t.Fatalf("item %d refused", i)
		}
	}
	if got := m.Computes(); got > 1 {
		t.Fatalf("%d computes for one batch, want 1", got)
	}
	if got := len(m.Compute().Member); got != 2001 {
		t.Fatalf("%d members, want 2001", got)
	}
	// Hearing it all again costs no compute at all.
	before := m.Computes()
	for _, ok := range m.AddBatch(items) {
		if ok {
			t.Fatal("stored a known item")
		}
	}
	if got := m.Computes() - before; got != 0 {
		t.Fatalf("%d computes for a re-sync", got)
	}
}

func TestAddKnownItemCostsNoCompute(t *testing.T) {
	root, guest := newEthKey(t), newEthKey(t)
	m := members.Open(t.TempDir()+"/m.json", []string{root.addr}, nil)
	it := root.invite(guest.addr, 1)
	if !m.Add(it) {
		t.Fatal("refused")
	}
	before := m.Computes()
	if m.Add(it) {
		t.Fatal("stored twice")
	}
	if m.Computes() != before {
		t.Fatal("a known item ran a compute")
	}
}

// A batch applied to any prior state must store exactly what the same items would
// through Add one at a time, and leave the same member set. The prior state is
// what makes it hard: gossip delivers some invites before the sync does, so batch
// items fall anywhere among the stored ones.
func TestAddBatchMatchesOneAtATime(t *testing.T) {
	for seed := range uint64(30) {
		rng := rand.New(rand.NewPCG(seed, 7))
		root := newEthKey(t)
		accounts := []ethKey{root}
		for range 40 {
			accounts = append(accounts, newEthKey(t))
		}
		var items []members.Item
		for range 200 {
			from := accounts[rng.IntN(len(accounts))]
			to := accounts[1+rng.IntN(len(accounts)-1)]
			if from.addr == to.addr {
				continue
			}
			items = append(items, from.invite(to.addr, int64(1+rng.IntN(1000))))
		}
		// A revocation now and then.
		if seed%3 == 0 {
			items = append(items, root.revoke(accounts[1+rng.IntN(len(accounts)-1)].addr, 5))
		}
		ever := map[string]bool{accounts[1+rng.IntN(len(accounts)-1)].addr: true}
		served := func() map[string]bool { return ever }

		// Gossip delivered some of them first, in arrival order.
		slow := members.Open(t.TempDir()+"/slow.json", []string{root.addr}, served)
		fast := members.Open(t.TempDir()+"/fast.json", []string{root.addr}, served)
		prior := items[:len(items)/4]
		rest := items[len(items)/4:]
		for _, it := range prior {
			a, b := slow.Add(it), fast.Add(it)
			if a != b {
				t.Fatal("two stores disagree")
			}
		}
		if got := fast.AddBatch(rest); len(got) != len(rest) {
			t.Fatalf("%d results for %d items", len(got), len(rest))
		}
		sorted := append([]members.Item(nil), rest...)
		sort.SliceStable(sorted, func(a, b int) bool {
			ra, rb := sorted[a].Revoke != nil, sorted[b].Revoke != nil
			if ra != rb {
				return ra
			}
			if ra {
				return sorted[a].Revoke.Time < sorted[b].Revoke.Time
			}
			if sorted[a].Invite.Time != sorted[b].Invite.Time {
				return sorted[a].Invite.Time < sorted[b].Invite.Time
			}
			return sorted[a].Invite.Inviter+">"+sorted[a].Invite.Invitee < sorted[b].Invite.Inviter+">"+sorted[b].Invite.Invitee
		})
		for _, it := range sorted {
			slow.Add(it)
		}
		if a, b := itemIDs(slow.Items()), itemIDs(fast.Items()); !reflect.DeepEqual(a, b) {
			t.Fatalf("seed %d: stored sets differ: %d vs %d items", seed, len(a), len(b))
		}
		sameSet(t, "compute", slow.Compute(), fast.Compute())
	}
}

func TestAddBatchReportsByPosition(t *testing.T) {
	root, guest, stranger := newEthKey(t), newEthKey(t), newEthKey(t)
	m := members.Open(t.TempDir()+"/m.json", []string{root.addr}, nil)
	bad := root.invite(newEthKey(t).addr, 3)
	bad.Invite.Sig = "00"
	got := m.AddBatch([]members.Item{
		stranger.invite(newEthKey(t).addr, 1),
		{},
		bad,
		guest.invite(stranger.addr, 2), // sorts after the invite that makes guest a member
		root.invite(guest.addr, 1),
	})
	if want := []bool{false, false, false, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := m.AddBatch(nil); len(got) != 0 {
		t.Fatalf("empty batch: %v", got)
	}
}

func TestAddBatchWithMembershipOff(t *testing.T) {
	a, b := newEthKey(t), newEthKey(t)
	m := members.Open(t.TempDir()+"/m.json", nil, nil)
	got := m.AddBatch([]members.Item{a.invite(b.addr, 1), a.revoke(b.addr, 2)})
	if !got[0] || got[1] {
		t.Fatalf("got %v, want the invite stored and the revocation refused", got)
	}
	if m.Computes() != 0 {
		t.Fatal("computed a member set with membership off")
	}
}

func TestOpenReplaysWithoutWriting(t *testing.T) {
	root := newEthKey(t)
	path := t.TempDir() + "/m.json"
	items := tree(t, root, 200, members.MemberInvites)
	first := members.Open(path, []string{root.addr}, nil)
	first.AddBatch(items)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if first.Writes() != 1 {
		t.Fatalf("%d writes, want 1", first.Writes())
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	m := members.Open(path, []string{root.addr}, nil)
	if got := m.Computes(); got > 1 {
		t.Fatalf("%d computes on open, want 1", got)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.Writes(); got != 0 {
		t.Fatalf("%d writes for a clean replay", got)
	}
	again, _ := os.Stat(path)
	if !again.ModTime().Equal(stat.ModTime()) {
		t.Fatal("the file changed")
	}
	sameSet(t, "reload", first.Compute(), m.Compute())
}

func TestOpenWritesOnceToDropJunk(t *testing.T) {
	root, stranger := newEthKey(t), newEthKey(t)
	path := t.TempDir() + "/m.json"
	good := tree(t, root, 20, members.MemberInvites)
	items := append(append([]members.Item{}, good...), stranger.invite(newEthKey(t).addr, 1), members.Item{})
	b, _ := json.Marshal(items)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	m := members.Open(path, []string{root.addr}, nil)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Writes() != 1 || len(m.Items()) != len(good) {
		t.Fatalf("%d writes, %d items", m.Writes(), len(m.Items()))
	}
	if got := members.Open(path, []string{root.addr}, nil); len(got.Items()) != len(good) {
		t.Fatalf("file kept %d items", len(got.Items()))
	}
}

// A file a past version or a crash left must not stop the node from starting.
func TestBadFilesDoNotStopTheNode(t *testing.T) {
	root := newEthKey(t)
	for _, content := range []string{"not json", "null", "[{}]", `[{"invite":null,"revoke":null}]`, `[{"invite":{}}, {"revoke":{}}, {}]`} {
		path := t.TempDir() + "/m.json"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		m := members.Open(path, []string{root.addr}, nil)
		if len(m.Items()) != 0 {
			t.Fatalf("%q: %d items", content, len(m.Items()))
		}
		if !m.Add(root.invite(newEthKey(t).addr, 1)) {
			t.Fatalf("%q: real invite refused", content)
		}
		m.Close()
	}
}

// Revoking prunes what no longer counts at once, and the file with it.
func TestRevokePrunesAtOnce(t *testing.T) {
	root, a, b, kept := newEthKey(t), newEthKey(t), newEthKey(t), newEthKey(t)
	path := t.TempDir() + "/m.json"
	m := members.Open(path, []string{root.addr}, nil)
	for _, it := range []members.Item{root.invite(a.addr, 1), root.invite(kept.addr, 2), a.invite(b.addr, 3), b.invite(newEthKey(t).addr, 4)} {
		if !m.Add(it) {
			t.Fatal("refused")
		}
	}
	if !m.Add(root.revoke(a.addr, 5)) {
		t.Fatal("revoke refused")
	}
	got := m.Items()
	if len(got) != 2 { // the revocation and root's invite to kept
		t.Fatalf("%d items after revoke: %+v", len(got), got)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	var onDisk []members.Item
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &onDisk); err != nil || len(onDisk) != 2 {
		t.Fatalf("file holds %d items: %v", len(onDisk), err)
	}
	if !m.Compute().Member[kept.addr] {
		t.Fatal("pruned a live invite")
	}
	// Someone caught under a can be invited again.
	if !m.Add(root.invite(b.addr, 6)) || !m.Compute().Member[b.addr] {
		t.Fatal("re-invite after prune failed")
	}
}

func TestBatchedRevokeFreesRoomForItsInvites(t *testing.T) {
	root, a, b, c := newEthKey(t), newEthKey(t), newEthKey(t), newEthKey(t)
	m := members.Open(t.TempDir()+"/m.json", []string{root.addr}, nil)
	m.AddBatch([]members.Item{root.invite(a.addr, 1), a.invite(b.addr, 2)})
	got := m.AddBatch([]members.Item{root.revoke(a.addr, 3), root.invite(c.addr, 4)})
	if !got[0] || !got[1] {
		t.Fatalf("got %v", got)
	}
	ms := m.Compute()
	if ms.Member[a.addr] || ms.Member[b.addr] || !ms.Member[c.addr] {
		t.Fatalf("after batch: %+v", ms.Member)
	}
	if n := len(m.Items()); n != 2 {
		t.Fatalf("%d items", n)
	}
}

// Fifty thousand invites through a tree: startup and a full sync take seconds,
// where a compute and a file write per item would take about an hour.
func TestMembershipAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("signs 50k invites")
	}
	if raceDetector {
		t.Skip("its bounds are wall-clock times, and the race detector makes signature checks many times slower")
	}
	const bound = 120 * time.Second
	root := newEthKey(t)
	roots := []string{root.addr}
	began := time.Now()
	// One member at the top, so its revocation below removes everything else.
	top := newEthKey(t)
	sub := tree(t, top, members.MaxInvites-1, members.MemberInvites)
	items := append([]members.Item{root.invite(top.addr, 1)}, sub...)
	t.Logf("generated %d invites in %v", len(items), time.Since(began))

	path := t.TempDir() + "/m.json"
	raw, _ := json.Marshal(items)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	m := members.Open(path, roots, nil)
	opened := time.Since(start)
	t.Logf("startup with %d invites: %v, %d computes, %d writes", len(items), opened, m.Computes(), m.Writes())
	if len(m.Items()) != len(items) || opened > bound {
		t.Fatalf("startup: %d items in %v", len(m.Items()), opened)
	}
	if m.Computes() > 1 || m.Writes() != 0 {
		t.Fatalf("startup: %d computes, %d writes", m.Computes(), m.Writes())
	}

	// A fresh node hears it all in one sync.
	fresh := members.Open(t.TempDir()+"/fresh.json", roots, nil)
	start = time.Now()
	fresh.AddBatch(items)
	first := time.Since(start)
	t.Logf("first sync: %v, %d computes", first, fresh.Computes())
	if len(fresh.Items()) != len(items) || first > bound || fresh.Computes() > 1 {
		t.Fatalf("first sync: %d items in %v, %d computes", len(fresh.Items()), first, fresh.Computes())
	}

	// A re-sync of the same items is close to free.
	before := fresh.Computes()
	start = time.Now()
	fresh.AddBatch(items)
	again := time.Since(start)
	t.Logf("re-sync: %v, %d computes", again, fresh.Computes()-before)
	if again > 10*time.Second || fresh.Computes() != before {
		t.Fatalf("re-sync: %v, %d computes", again, fresh.Computes()-before)
	}

	// Gossip got one invite in first: every synced invite now falls before it.
	gossiped := members.Open(t.TempDir()+"/gossip.json", roots, nil)
	late := newEthKey(t)
	if !gossiped.Add(root.invite(late.addr, time.Now().Unix())) {
		t.Fatal("refused")
	}
	before = gossiped.Computes()
	start = time.Now()
	gossiped.AddBatch(items[:len(items)-1]) // the store holds MaxInvites
	took := time.Since(start)
	t.Logf("sync behind a gossiped invite: %v, %d computes", took, gossiped.Computes()-before)
	if took > bound || gossiped.Computes()-before > 2 {
		t.Fatalf("sync behind a gossiped invite: %v, %d computes", took, gossiped.Computes()-before)
	}

	// The store is full: nothing new fits.
	guest := newEthKey(t)
	if m.Add(root.invite(guest.addr, time.Now().Unix())) {
		t.Fatal("stored past MaxInvites")
	}
	// The root removes the one subtree, and there is room without a restart.
	start = time.Now()
	if !m.Add(root.revoke(top.addr, 2)) {
		t.Fatal("revoke refused")
	}
	t.Logf("revoke of a %d-invite subtree: %v", len(sub), time.Since(start))
	if n := len(m.Items()); n != 1 { // only the revocation is left
		t.Fatalf("%d items after the revoke", n)
	}
	if !m.Add(root.invite(guest.addr, time.Now().Unix())) || !m.Compute().Member[guest.addr] {
		t.Fatal("no room for a new invite after the revoke")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Size() > 4<<10 {
		t.Fatalf("file is %d bytes after pruning", fi.Size())
	}
}

// The limit reads the served set each time it computes.
func TestLimitFollowsServedSet(t *testing.T) {
	root, a := newEthKey(t), newEthKey(t)
	var calls atomic.Int64
	ever := map[string]bool{}
	m := members.Open(t.TempDir()+"/m.json", []string{root.addr}, func() map[string]bool {
		calls.Add(1)
		return ever
	})
	m.Add(root.invite(a.addr, 1))
	if got := m.Compute().InviteLimit(a.addr); got != members.MemberInvites {
		t.Fatalf("limit %d", got)
	}
	ever[a.addr] = true
	if got := m.Compute().InviteLimit(a.addr); got != members.ContributorInvites {
		t.Fatalf("limit %d", got)
	}
	if calls.Load() == 0 {
		t.Fatal("never asked")
	}
}
