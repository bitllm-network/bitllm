package node

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/members"
	"github.com/bitllm-org/bitllm/internal/receipts"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
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

// claimNode makes a node key owned by account and records the claim on n.
func claimNode(t *testing.T, n *Node, owner ethKey) (peer.ID, crypto.PrivKey) {
	nk, _, _ := crypto.GenerateEd25519Key(nil)
	id, _ := peer.IDFromPrivateKey(nk)
	c, err := account.NewClaim(id, owner.addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.OwnerSig = hex.EncodeToString(account.EthSign(owner.k, c.Message()))
	if err := account.SignClaimAsNode(c, nk); err != nil {
		t.Fatal(err)
	}
	if !n.claims.Add(*c) {
		t.Fatal("claim refused")
	}
	return id, nk
}

func TestAccessRules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, member, contrib := newEthKey(t), newEthKey(t), newEthKey(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}, FreeDaily: 30, Concurrency: 2})
	if !n.members.Add(root.invite(member.addr, 1)) || !n.members.Add(root.invite(contrib.addr, 2)) {
		t.Fatal("invites refused")
	}
	memberNode, _ := claimNode(t, n, member)
	contribNode, contribKey := claimNode(t, n, contrib)
	stranger := randomPeer(t)
	now := time.Now()

	// Rule 3: a contributor served a job recently (a receipt names its node as server).
	ck, _, _ := crypto.GenerateEd25519Key(nil)
	cid, _ := peer.IDFromPrivateKey(ck)
	r := receipts.Receipt{Job: "j1", Model: "m", Client: cid.String(), Server: contribNode.String(),
		Prompt: 4, Completion: 1, Credits: receipts.Price(4, 1), Output: "x", TS: now.Unix()}
	sr := receipts.SignedReceipt{Receipt: r}
	sr.ClientSig, _ = ck.Sign(r.Bytes())
	sr.ServerSig, _ = contribKey.Sign(r.Bytes())
	if !n.receipts.Add(sr) {
		t.Fatal("receipt refused")
	}
	n.invalidateMembers()

	release := func() { n.inflight.Add(-1) }

	// Rule 2: a member is served until the daily quota, then refused.
	s, _, adm := n.admit(memberNode, "", now, 0, 0)
	tk := adm.t
	if s != 0 || tk.key != member.addr {
		t.Fatalf("member refused: %d %+v", s, tk)
	}
	n.done(tk, 30, now) // uses up the quota
	if s, msg, _ := n.admit(memberNode, "", now, 0, 0); s != 429 || msg != msgQuotaSpent {
		t.Fatalf("member over quota: %d %s", s, msg)
	}
	if s, _, _ := n.admit(memberNode, "", now.Add(24*time.Hour), 0, 0); s != 0 {
		t.Fatal("quota did not reset the next UTC day")
	}
	release()

	// Rule 3: a contributor has no quota.
	n.quota.Add(contrib.addr, 1_000_000, now)
	s, _, adm = n.admit(contribNode, "", now, 0, 0)
	tk = adm.t
	if s != 0 || tk.key != "" {
		t.Fatalf("contributor: %d %+v", s, tk)
	}
	release()

	// Rule 1: uninvited only when idle.
	if s, _, _ := n.admit(stranger, "", now, 0, 0); s != 0 {
		t.Fatal("uninvited refused on an idle server")
	}
	if s, msg, _ := n.admit(stranger, "", now, 0, 0); s != 429 || !isBusy(s, msg) {
		t.Fatalf("uninvited served on a busy server: %d %s", s, msg)
	}
	release()
	// A member still gets the second slot while the stranger holds one.
	if s, _, _ := n.admit(stranger, "", now, 0, 0); s != 0 {
		t.Fatal("idle again")
	}
	if s, _, _ := n.admit(contribNode, "", now, 0, 0); s != 0 {
		t.Fatal("contributor refused a free slot")
	}
	release()
	release()
}

// TestInviteGossipAndSync: an invite made on one node reaches the bootstrap
// over gossip and a node that joins later through sync.
func TestInviteGossipAndSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, guest := newEthKey(t), newEthKey(t)
	cfg := func() Config { return Config{Roots: []string{root.addr}} }
	boot := testNode(t, ctx, cfg())
	bs := boot.Addrs()[:1]
	c := cfg()
	c.Bootstrap = bs
	a := testNode(t, ctx, c)
	for deadline := time.Now().Add(15 * time.Second); ; {
		if a.publishMember(root.invite(guest.addr, time.Now().Unix())) || a.members.Compute().Member[guest.addr] {
			if boot.members.Compute().Member[guest.addr] {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("invite never reached the bootstrap")
		}
		time.Sleep(200 * time.Millisecond)
	}
	late := cfg()
	late.Bootstrap = bs
	if !testNode(t, ctx, late).members.Compute().Member[guest.addr] {
		t.Fatal("late joiner did not sync the invite")
	}
}

// TestOpenServer: --open serves uninvited nodes on spare slots (at most
// OpenSlots of them, so members always find room), with a per-node quota and
// a shared daily pool for all uninvited nodes together.
func TestOpenServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, member := newEthKey(t), newEthKey(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}, Open: true, OpenSlots: 2, OpenDaily: 100,
		FreeDaily: 30, Concurrency: 3})
	n.members.Add(root.invite(member.addr, 1))
	memberNode, _ := claimNode(t, n, member)
	a, b, c := randomPeer(t), randomPeer(t), randomPeer(t)
	now := time.Now()

	s1, _, adm := n.admit(a, "", now, 0, 0)
	ta := adm.t
	s2, _, adm := n.admit(b, "", now, 0, 0)
	tb := adm.t
	if s1 != 0 || s2 != 0 || ta.key != "node:"+a.String() || !ta.open {
		t.Fatalf("open server refused uninvited on spare slots: %d %d %+v", s1, s2, ta)
	}
	// The third slot is kept for members.
	if s, msg, _ := n.admit(c, "", now, 0, 0); s != 429 || msg != msgUninvited {
		t.Fatalf("uninvited took a reserved slot: %d %s", s, msg)
	}
	sm, _, adm := n.admit(memberNode, "", now, 0, 0)
	tm := adm.t
	if sm != 0 || tm.open {
		t.Fatalf("member refused the reserved slot: %d", sm)
	}
	n.done(tm, 0, now)
	n.done(tb, 0, now)

	// Per-node quota: a has used 30, b has not.
	n.done(ta, 30, now)
	if s, msg, _ := n.admit(a, "", now, 0, 0); s != 429 || msg != msgQuotaSpent {
		t.Fatalf("per-node quota not applied: %d %s", s, msg)
	}
	sb, _, adm := n.admit(b, "", now, 0, 0)
	tb = adm.t
	if sb != 0 {
		t.Fatal("one node's quota limited another")
	}
	// The shared pool (100) runs out after 30 + 70 = 100: fresh ids get idle-only.
	n.done(tb, 70, now)
	sc, _, adm := n.admit(c, "", now, 0, 0)
	tc := adm.t // idle server: served, but not as open
	if sc != 0 || tc.open {
		t.Fatalf("pool spent: want idle-only service, got %d %+v", sc, tc)
	}
	if s, msg, _ := n.admit(randomPeer(t), "", now, 0, 0); s != 429 || msg != msgUninvited {
		t.Fatalf("pool spent and busy: want refusal, got %d %s", s, msg)
	}
	n.done(tc, 0, now)
	if n.inflight.Load() != 0 || n.openInflight.Load() != 0 {
		t.Fatalf("slots leaked: %d %d", n.inflight.Load(), n.openInflight.Load())
	}
}
