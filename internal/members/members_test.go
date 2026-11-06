package members_test

import (
	"encoding/hex"
	"testing"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/members"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

type ethKey struct {
	k    *secp256k1.PrivateKey
	addr string
}

func newEthKey(t *testing.T) ethKey {
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return ethKey{k, account.EthAddress(k.PubKey())}
}

func (e ethKey) invite(to string, at int64) members.Item {
	i := &members.Invite{Inviter: e.addr, Invitee: to, Time: at}
	i.Sig = hex.EncodeToString(account.EthSign(e.k, i.Message()))
	return members.Item{Invite: i}
}

func (e ethKey) revoke(acct string, at int64) members.Item {
	r := &members.Revoke{Account: acct, Root: e.addr, Time: at}
	r.Sig = hex.EncodeToString(account.EthSign(e.k, r.Message()))
	return members.Item{Revoke: r}
}

func TestMemberSet(t *testing.T) {
	root, a, b, c := newEthKey(t), newEthKey(t), newEthKey(t), newEthKey(t)
	ever := map[string]bool{}
	served := func() map[string]bool { return ever }
	path := t.TempDir() + "/m.json"
	m := members.Open(path, []string{root.addr}, served)
	add := func(it members.Item) {
		t.Helper()
		if err := m.Check(it); err != nil || !m.Add(it) {
			t.Fatalf("valid item refused: %v", err)
		}
	}
	add(root.invite(a.addr, 100))
	add(a.invite(b.addr, 200))
	if m.Add(c.invite(a.addr, 50)) || m.Add(c.invite(newEthKey(t).addr, 300)) {
		t.Fatal("stored an invite from a non-member")
	}
	ms := m.Compute()
	if !ms.Member[a.addr] || !ms.Member[b.addr] || ms.Member[c.addr] || ms.InvitedBy[b.addr] != a.addr {
		t.Fatalf("chain: %+v", ms)
	}

	// A member stores 3 invites; once it has served, 10.
	var extra []string
	tryInvites := func(from ethKey, n int, at int64) (stored int) {
		for i := range n {
			x := newEthKey(t).addr
			extra = append(extra, x)
			if m.Add(from.invite(x, at+int64(i))) {
				stored++
			}
		}
		return
	}
	if got := tryInvites(b, 5, 300); got != members.MemberInvites {
		t.Fatalf("member stored %d invites, want %d", got, members.MemberInvites)
	}
	ever[b.addr] = true
	if got := tryInvites(b, 10, 400); got != members.ContributorInvites-members.MemberInvites {
		t.Fatalf("sharer stored %d more, want %d", got, members.ContributorInvites-members.MemberInvites)
	}
	membersOf := func() (n int) {
		ms := m.Compute()
		for _, x := range extra {
			if ms.Member[x] {
				n++
			}
		}
		return
	}
	// The limit never drops, so all of them still count.
	if got := membersOf(); got != members.ContributorInvites {
		t.Fatalf("%d of b's invitees are members, want %d", got, members.ContributorInvites)
	}
	if got := tryInvites(root, 12, 500); got != 12 {
		t.Fatalf("root stored %d of 12", got)
	}

	// Revoking a removes a, b (invited by a) and everyone b invited.
	d := newEthKey(t)
	add(a.invite(d.addr, 600))
	add(root.revoke(a.addr, 700))
	ms = m.Compute()
	if ms.Member[a.addr] || ms.Member[b.addr] || ms.Member[d.addr] {
		t.Fatal("revoke did not cascade")
	}
	for _, x := range extra[:15] { // b's invitees
		if ms.Member[x] {
			t.Fatal("revoke left a grandchild")
		}
	}
	// A revoked account cannot invite, not even backdated.
	if m.Add(a.invite(newEthKey(t).addr, 150)) {
		t.Fatal("stored a backdated invite from a revoked account")
	}
	// Someone else can re-invite a person caught under a.
	add(root.invite(d.addr, 800))
	if !m.Compute().Member[d.addr] {
		t.Fatal("re-invite after revoke failed")
	}
	if m.Add(b.revoke(root.addr, 900)) || m.Add(root.revoke(root.addr, 900)) {
		t.Fatal("bad revoke accepted")
	}
	bad := root.invite(c.addr, 1000)
	bad.Invite.Inviter = d.addr
	if m.Add(bad) {
		t.Fatal("forged invite accepted")
	}
	// A reload replays to the same state.
	before := m.Compute()
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	again := members.Open(path, []string{root.addr}, served).Compute()
	if len(again.Member) != len(before.Member) || again.Member[a.addr] || !again.Member[d.addr] {
		t.Fatalf("reload: %d vs %d members", len(again.Member), len(before.Member))
	}
}

// TestInviteSpamCannotBlock: invites signed by outsiders are not stored, so
// they cannot fill the store and keep real invites out.
func TestInviteSpamCannotBlock(t *testing.T) {
	root := newEthKey(t)
	m := members.Open(t.TempDir()+"/m.json", []string{root.addr}, nil)
	for i := range 1000 {
		m.Add(newEthKey(t).invite(newEthKey(t).addr, int64(i)))
	}
	if n := len(m.Items()); n != 0 {
		t.Fatalf("stored %d junk invites", n)
	}
	guest := newEthKey(t)
	if !m.Add(root.invite(guest.addr, 2000)) || !m.Compute().Member[guest.addr] {
		t.Fatal("real invite refused")
	}
}
