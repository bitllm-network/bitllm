package node

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/members"
	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/bitllm-org/bitllm/internal/receipts"
	"github.com/bitllm-org/bitllm/internal/store"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2pproto "github.com/libp2p/go-libp2p/core/protocol"
)

// signedClaim makes a claim of a new node by owner, without storing it.
func signedClaim(t *testing.T, owner ethKey, at int64) (account.Claim, peer.ID, crypto.PrivKey) {
	t.Helper()
	nk, _, _ := crypto.GenerateEd25519Key(nil)
	id, _ := peer.IDFromPrivateKey(nk)
	c, err := account.NewClaim(id, owner.addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Time = at
	c.OwnerSig = hex.EncodeToString(account.EthSign(owner.k, c.Message()))
	if err := account.SignClaimAsNode(c, nk); err != nil {
		t.Fatal(err)
	}
	return *c, id, nk
}

// linkAsLocal does what POST /account/link does for this node.
func linkAsLocal(t *testing.T, n *Node, owner ethKey) error {
	t.Helper()
	at := time.Now().Unix()
	c, err := account.NewClaim(n.host.ID(), owner.addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Time = at
	_, err = n.Link(owner.addr, "", at, hex.EncodeToString(account.EthSign(owner.k, c.Message())))
	return err
}

// floodClaims fills the store from accounts nobody invited, as fast as the cores
// allow.
func floodClaims(t *testing.T, n *Node, count int) {
	t.Helper()
	store.ForEach(count, func(int) {
		k, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			panic(err)
		}
		c, _, _ := signedClaim(t, ethKey{k, account.EthAddress(k.PubKey())}, time.Now().Unix())
		n.claims.Add(c)
	})
}

// TestClaimFloodCannotHurtMembers: claims from free keys cannot stop a new node
// linking, cannot evict a member's claim, and a claim that came before its invite
// still counts once the invite is there.
func TestClaimFloodCannotHurtMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("signs more claims than the store holds")
	}
	ctx := t.Context()
	root, member, early, owner := newEthKey(t), newEthKey(t), newEthKey(t), newEthKey(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}})
	if !n.members.Add(root.invite(member.addr, 1)) {
		t.Fatal("invite refused")
	}
	n.invalidateMembers()
	memberNode, _ := claimNode(t, n, member)
	earlyClaim, earlyNode, _ := signedClaim(t, early, time.Now().Unix())
	if !n.claims.Add(earlyClaim) {
		t.Fatal("early claim refused")
	}
	// The early claim's owner joins while it is still among the newest arrivals.
	floodClaims(t, n, 100)
	if !n.publishMember(root.invite(early.addr, 2)) {
		t.Fatal("invite refused")
	}

	floodClaims(t, n, account.MaxClaims+500)

	if got := len(n.claims.All()); got > account.MaxClaims {
		t.Fatalf("%d claims, over the cap", got)
	}
	if err := linkAsLocal(t, n, owner); err != nil {
		t.Fatalf("this node could not link: %v", err)
	}
	if n.accountOf(n.host.ID()) != owner.addr {
		t.Fatal("own claim not stored")
	}
	if n.accountOf(memberNode) != member.addr {
		t.Fatal("the flood evicted a member's claim")
	}
	if n.accountOf(earlyNode) != early.addr {
		t.Fatal("the flood evicted a claim that came before its invite")
	}
	if s, _, adm := n.admit(memberNode, "", time.Now(), 0, 0); s != 0 {
		t.Fatalf("member refused after the flood: %d", s)
	} else {
		n.done(adm.t, 0, time.Now())
	}
}

// TestClaimBeforeInviteAfterRestart: a claim that came before its invite is still
// there and counted after a restart.
func TestClaimBeforeInviteAfterRestart(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	root, guest := newEthKey(t), newEthKey(t)
	dir := t.TempDir()
	cfg := Config{Roots: []string{root.addr}, Listen: []string{"/ip4/127.0.0.1/tcp/0"}, DataDir: dir, DHTServer: true,
		AnnounceEvery: 300 * time.Millisecond}
	n, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, id, _ := signedClaim(t, guest, time.Now().Unix())
	if !n.claims.Add(c) {
		t.Fatal("claim refused")
	}
	if !n.publishMember(root.invite(guest.addr, 1)) {
		t.Fatal("invite refused")
	}
	n.flushState() // nothing else writes these before the timer does
	n.host.Close()
	n.closeState()
	stop()

	ctx2 := t.Context()
	again, err := New(ctx2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.host.Close(); again.closeState() })
	if again.accountOf(id) != guest.addr || !again.memberSet().Member[guest.addr] {
		t.Fatal("claim or invite lost across a restart")
	}
}

// TestStoresDoNotDeadlock runs what took the locks in the wrong order: a member
// set computing through claims while claims ask about the member set.
func TestStoresDoNotDeadlock(t *testing.T) {
	ctx := t.Context()
	root, guest := newEthKey(t), newEthKey(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}})
	claims := make([]account.Claim, 40)
	for i := range claims {
		claims[i], _, _ = signedClaim(t, guest, int64(i+1))
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for _, c := range claims {
			n.claims.Add(c)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 40 {
			n.publishMember(root.invite(newEthKey(t).addr, int64(i+1)))
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			n.memberSet()
			n.dropCachedSets()
			n.contributors()
		}
	}()
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock between the claim store and the member set")
	}
}

// signedByPair makes a receipt between two keys of the caller's choosing.
func signedByPair(t *testing.T, clientKey, serverKey crypto.PrivKey, ts, tokens int64) receipts.SignedReceipt {
	t.Helper()
	cid, _ := peer.IDFromPrivateKey(clientKey)
	sid, _ := peer.IDFromPrivateKey(serverKey)
	r := receipts.Receipt{Job: fmt.Sprintf("%s-%d", cid.String()[8:20], time.Now().UnixNano()), Model: "m",
		Client: cid.String(), Server: sid.String(), Prompt: 0, Completion: tokens, Credits: receipts.Price(0, tokens), Output: "x", TS: ts}
	sr := receipts.SignedReceipt{Receipt: r}
	sr.ClientSig, _ = clientKey.Sign(r.Bytes())
	sr.ServerSig, _ = serverKey.Sign(r.Bytes())
	return sr
}

// ourReceipt is a job node n really asked of server: signed by n's pseudonym for it.
func ourReceipt(t *testing.T, n *Node, server peer.ID, serverKey crypto.PrivKey, at time.Time, tokens int64) receipts.SignedReceipt {
	t.Helper()
	ck, err := receipts.Key(n.receiptSeed, server, at)
	if err != nil {
		t.Fatal(err)
	}
	return signedByPair(t, ck, serverKey, at.Unix(), tokens)
}

func exempt(t *testing.T, n *Node, p peer.ID) bool {
	t.Helper()
	n.dropCachedSets()
	s, msg, adm := n.admit(p, "", time.Now(), 0, 0)
	if s != 0 {
		t.Fatalf("admit: %d %s", s, msg)
	}
	n.done(adm.t, 0, time.Now())
	return adm.t.key == ""
}

// TestQuotaExemptionNeedsOurOwnProof: only a job this node asked for exempts an
// account, recently, and none of it touches the invite limit.
func TestQuotaExemptionNeedsOurOwnProof(t *testing.T) {
	ctx := t.Context()
	root, member, other := newEthKey(t), newEthKey(t), newEthKey(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}, FreeDaily: 30})
	n.members.Add(root.invite(member.addr, 1))
	n.members.Add(root.invite(other.addr, 2))
	node, nodeKey := claimNode(t, n, member)
	otherNode, otherKey := claimNode(t, n, other)
	now := time.Now()

	limit := func() int { n.dropCachedSets(); return n.memberSet().InviteLimit(member.addr) }
	if limit() != members.MemberInvites {
		t.Fatalf("limit %d before any receipt", limit())
	}

	// Two keys of a stranger's own make a valid receipt that proves nothing here.
	strangerKey, _, _ := crypto.GenerateEd25519Key(nil)
	if !n.receipts.Add(signedByPair(t, strangerKey, nodeKey, now.Unix(), 100)) {
		t.Fatal("receipt refused")
	}
	if exempt(t, n, node) {
		t.Fatal("a self-minted receipt gave a quota exemption")
	}
	// It still counts for the public record, which all nodes must agree on.
	if limit() != members.ContributorInvites {
		t.Fatalf("limit %d after a public receipt", limit())
	}

	// A job that earned nothing proves nothing.
	if !n.receipts.Add(ourReceipt(t, n, node, nodeKey, now, 0)) {
		t.Fatal("receipt refused")
	}
	if exempt(t, n, node) {
		t.Fatal("a zero-credit job gave a quota exemption")
	}
	// One we asked for does.
	if !n.receipts.Add(ourReceipt(t, n, node, nodeKey, now, 10)) {
		t.Fatal("receipt refused")
	}
	if !exempt(t, n, node) {
		t.Fatal("a real job gave no exemption")
	}
	if !n.memberInfo(member.addr).Contributor {
		t.Fatal("memberInfo does not report the contributor")
	}
	if n.memberInfo(other.addr).Contributor {
		t.Fatal("memberInfo reports a contributor without proof")
	}

	// An old job has expired.
	if !n.receipts.Add(ourReceipt(t, n, otherNode, otherKey, now.Add(-members.ContributorWindow-time.Hour), 10)) {
		t.Fatal("receipt refused")
	}
	if exempt(t, n, otherNode) {
		t.Fatal("an old job still gave an exemption")
	}
	// That one still counts as having served, which never expires.
	if n.dropCachedSets(); n.memberSet().InviteLimit(other.addr) != members.ContributorInvites {
		t.Fatal("the invite limit dropped when the job expired")
	}

	// A receipt signed with another node's seed is someone else's proof.
	otherSeed := make([]byte, 32)
	ck, _ := receipts.Key(otherSeed, otherNode, now)
	if !n.receipts.Add(signedByPair(t, ck, otherKey, now.Unix(), 10)) {
		t.Fatal("receipt refused")
	}
	if exempt(t, n, otherNode) {
		t.Fatal("a receipt from another seed gave an exemption")
	}
}

// TestRealJobGivesExemption: the whole path, through a served job and its receipt.
func TestRealJobGivesExemption(t *testing.T) {
	ctx := t.Context()
	up := mockUpstream(t, "local-ds")
	t.Cleanup(up.Close)
	root, member := newEthKey(t), newEthKey(t)
	seed := testNode(t, ctx, Config{Roots: []string{root.addr}})
	bs := seed.Addrs()[:1]
	server := testNode(t, ctx, Config{Roots: []string{root.addr}, Bootstrap: bs, Open: true, Upstream: up.URL + "/v1",
		Models: map[string]string{"ds": "local-ds"}})
	client := testNode(t, ctx, Config{Roots: []string{root.addr}, Bootstrap: bs, FreeDaily: 30})

	// The server's node belongs to an account the client knows as a member.
	if !client.members.Add(root.invite(member.addr, 1)) {
		t.Fatal("invite refused")
	}
	c, err := account.NewClaim(server.host.ID(), member.addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c.OwnerSig = hex.EncodeToString(account.EthSign(member.k, c.Message()))
	if err := account.SignClaimAsNode(c, server.nodeKey); err != nil {
		t.Fatal(err)
	}
	if !client.claims.Add(*c) {
		t.Fatal("claim refused")
	}
	client.invalidateMembers()

	if exempt(t, client, server.host.ID()) {
		t.Fatal("exempt before any job")
	}
	// A receipt is gossiped once, and gossip sent before the mesh forms is lost,
	// so keep asking until one has come back to the client.
	eventually(t, 30*time.Second, "no job gave an exemption", func() bool {
		post(client, `{"model":"ds","messages":[]}`)
		return exempt(t, client, server.host.ID())
	})
}

// seedReceipts stores count fresh receipts on n, from one forged pair.
func seedReceipts(t *testing.T, n *Node, count int) (server string) {
	t.Helper()
	sk, _, _ := crypto.GenerateEd25519Key(nil)
	ck, _, _ := crypto.GenerateEd25519Key(nil)
	sid, _ := peer.IDFromPrivateKey(sk)
	cid, _ := peer.IDFromPrivateKey(ck)
	for i := range count {
		r := receipts.Receipt{Job: fmt.Sprintf("%s-%d", sid.String()[10:22], i), Model: "m", Client: cid.String(), Server: sid.String(),
			Completion: 1, Credits: 1, Output: "x", TS: time.Now().Unix()}
		sr := receipts.SignedReceipt{Receipt: r}
		sr.ClientSig, _ = ck.Sign(r.Bytes())
		sr.ServerSig, _ = sk.Sign(r.Bytes())
		if !n.receipts.Add(sr) {
			t.Fatal("receipt refused")
		}
	}
	return sid.String()
}

func connected(t *testing.T, a, b *Node) {
	t.Helper()
	if err := a.host.Connect(t.Context(), peer.AddrInfo{ID: b.host.ID(), Addrs: b.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
}

// TestSyncContinuesFromACursor: the wire format, and that a sync delivers each
// receipt once, in chunks, resuming where the last one stopped.
func TestSyncContinuesFromACursor(t *testing.T) {
	ctx := t.Context()
	root, guest := newEthKey(t), newEthKey(t)
	seed := testNode(t, ctx, Config{Roots: []string{root.addr}})
	seed.syncGate.maxReceipts = 3
	seed.syncGate.every = time.Nanosecond
	seed.members.Add(root.invite(guest.addr, 1))
	claimed, _ := claimNode(t, seed, guest)
	server := seedReceipts(t, seed, 7)

	client := testNode(t, ctx, Config{Roots: []string{root.addr}})
	connected(t, client, seed)
	added, err := client.syncReceipts(ctx, seed.host.ID())
	if err != nil || added != 7 {
		t.Fatalf("first sync: %d receipts, %v", added, err)
	}
	if got := client.receipts.Tally()[server]; got.Jobs != 7 {
		t.Fatalf("tally %+v", got)
	}
	if client.accountOf(claimed) != guest.addr || !client.memberSet().Member[guest.addr] {
		t.Fatal("membership or claim did not arrive")
	}
	cur := client.syncCursors.get(seed.host.ID())
	if cur.ID != seed.receipts.StoreID() || cur.Since == 0 {
		t.Fatalf("cursor %+v", cur)
	}

	// Nothing new: no receipts, and the cursor holds.
	if added, err := client.syncReceipts(ctx, seed.host.ID()); err != nil || added != 0 {
		t.Fatalf("second sync: %d receipts, %v", added, err)
	}
	if got := client.syncCursors.get(seed.host.ID()); got != cur {
		t.Fatalf("cursor moved %+v -> %+v", cur, got)
	}

	// The cursor survives a restart of the client, in its small state file.
	reopened := openSyncCursors(filepath.Join(client.cfg.DataDir, "sync-cursors.json"))
	if got := reopened.get(seed.host.ID()); got != cur {
		t.Fatalf("cursor after reopen %+v, want %+v", got, cur)
	}

	// New receipts come alone.
	seedReceipts(t, seed, 2)
	if added, err := client.syncReceipts(ctx, seed.host.ID()); err != nil || added != 2 {
		t.Fatalf("third sync: %d receipts, %v", added, err)
	}
}

// readResponse opens sync 2.0.0 by hand and reads what comes back.
func readResponse(t *testing.T, from, to *Node, req string) []map[string]json.RawMessage {
	t.Helper()
	s, err := from.host.NewStream(t.Context(), to.host.ID(), protocol.SyncProto2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := s.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	s.CloseWrite()
	var out []map[string]json.RawMessage
	sc := bufio.NewScanner(s)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// TestSyncWireFormat pins what compat.sh and other implementers rely on.
func TestSyncWireFormat(t *testing.T) {
	ctx := t.Context()
	root, guest := newEthKey(t), newEthKey(t)
	seed := testNode(t, ctx, Config{Roots: []string{root.addr}})
	seed.syncGate.every = time.Nanosecond
	seed.syncGate.maxReceipts = 2
	seed.members.Add(root.invite(guest.addr, 1))
	claimNode(t, seed, guest)
	seedReceipts(t, seed, 3)
	client := testNode(t, ctx, Config{})
	connected(t, client, seed)

	lines := readResponse(t, client, seed, `{"since":0,"cursor_id":""}`+"\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines: %v", len(lines), lines)
	}
	if lines[0]["invite"] == nil || lines[1]["claim"] == nil {
		t.Fatalf("membership and claims do not come first: %v", lines[:2])
	}
	if lines[2]["receipt"] == nil || lines[3]["receipt"] == nil {
		t.Fatalf("receipts do not follow: %v", lines[2:4])
	}
	var end struct {
		Cursor  *int64 `json:"cursor"`
		More    bool   `json:"more"`
		StoreID string `json:"store_id"`
	}
	b, _ := json.Marshal(lines[4])
	if err := json.Unmarshal(b, &end); err != nil || end.Cursor == nil || !end.More || end.StoreID != seed.receipts.StoreID() {
		t.Fatalf("closing line %s: %v", b, err)
	}
	// With the cursor, the rest. Membership and claims come again regardless.
	next := fmt.Sprintf(`{"since":%d,"cursor_id":%q}`+"\n", *end.Cursor, end.StoreID)
	lines = readResponse(t, client, seed, next)
	if len(lines) != 4 || lines[0]["invite"] == nil || lines[1]["claim"] == nil || lines[2]["receipt"] == nil {
		t.Fatalf("continued: %v", lines)
	}
	// A cursor from another store starts over.
	lines = readResponse(t, client, seed, fmt.Sprintf(`{"since":%d,"cursor_id":"other"}`+"\n", *end.Cursor))
	if len(lines) != 5 {
		t.Fatalf("another store's cursor: %d lines", len(lines))
	}
}

func TestSyncGate(t *testing.T) {
	var g syncGate
	now := time.Now()
	a, b := randomPeer(t), randomPeer(t)
	if !g.enter(a, now) {
		t.Fatal("first sync refused")
	}
	if g.enter(a, now) {
		t.Fatal("two syncs at once from one peer")
	}
	g.leave(a, false)
	if g.enter(a, now.Add(59*time.Second)) {
		t.Fatal("a second sync inside the minute")
	}
	if !g.enter(a, now.Add(61*time.Second)) {
		t.Fatal("refused after the minute")
	}
	g.leave(a, true) // "more": it may come straight back
	later := now.Add(62 * time.Second)
	if !g.enter(a, later) {
		t.Fatal("continuing after more was refused")
	}
	g.leave(a, false)
	if g.enter(a, later) {
		t.Fatal("more outlived its one continuation")
	}
	// Four at once, overall.
	held := []peer.ID{randomPeer(t), randomPeer(t), randomPeer(t), randomPeer(t)}
	for _, p := range held {
		if !g.enter(p, later) {
			t.Fatal("refused under the limit")
		}
	}
	if g.enter(b, later) {
		t.Fatal("a fifth sync ran")
	}
	g.leave(held[0], false)
	if !g.enter(b, later) {
		t.Fatal("a freed slot stayed taken")
	}
}

func TestSyncGateIsBounded(t *testing.T) {
	var g syncGate
	now := time.Now()
	for range syncGatePeers {
		p := randomPeer(t)
		if !g.enter(p, now) {
			t.Fatal("refused under the cap")
		}
		g.leave(p, false)
	}
	if g.enter(randomPeer(t), now) {
		t.Fatal("the table grew past its cap")
	}
	if !g.enter(randomPeer(t), now.Add(2*syncPeerEvery)) {
		t.Fatal("expired entries were not cleared")
	}
	if len(g.last) > 1 {
		t.Fatalf("%d entries left", len(g.last))
	}
}

// TestOverLimitClosesWithNoData: a second sync inside the minute gets nothing on
// either id, and an honest client reports it as a failure to retry.
func TestOverLimitClosesWithNoData(t *testing.T) {
	ctx := t.Context()
	seed := testNode(t, ctx, Config{})
	seedReceipts(t, seed, 2)
	client := testNode(t, ctx, Config{})
	connected(t, client, seed)
	if _, err := client.syncReceipts(ctx, seed.host.ID()); err != nil {
		t.Fatal(err)
	}
	if got := readResponse(t, client, seed, `{"since":0,"cursor_id":""}`+"\n"); len(got) != 0 {
		t.Fatalf("over the limit on 2.0.0: %d lines", len(got))
	}
	s, err := client.host.NewStream(ctx, seed.host.ID(), protocol.SyncProto)
	if err != nil {
		t.Fatal(err)
	}
	s.CloseWrite()
	s.SetDeadline(time.Now().Add(10 * time.Second))
	if b, err := io.ReadAll(s); err != nil || len(b) != 0 {
		t.Fatalf("over the limit on the old id: %d bytes, %v", len(b), err)
	}
	if _, err := client.syncReceipts(ctx, seed.host.ID()); err == nil {
		t.Fatal("a refused sync looked like a finished one")
	}
}

func TestSyncRequestIsBounded(t *testing.T) {
	ctx := t.Context()
	seed := testNode(t, ctx, Config{})
	seed.syncGate.every = time.Nanosecond
	seedReceipts(t, seed, 1)
	client := testNode(t, ctx, Config{})
	connected(t, client, seed)

	// Bigger than the cap, then not JSON, then silence for the whole wait.
	big := `{"since":0,"cursor_id":"` + strings.Repeat("x", syncRequestBytes) + `"}` + "\n"
	if got := readResponse(t, client, seed, big); len(got) != 0 {
		t.Fatalf("oversized request answered with %d lines", len(got))
	}
	if got := readResponse(t, client, seed, "not json\n"); len(got) != 0 {
		t.Fatalf("garbage answered with %d lines", len(got))
	}
	s, err := client.host.NewStream(ctx, seed.host.ID(), protocol.SyncProto2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Reset()
	s.Write([]byte(`{"since":0`)) // no end of line, and the stream stays open
	s.SetDeadline(time.Now().Add(syncRequestWait + 10*time.Second))
	start := time.Now()
	b, err := io.ReadAll(s)
	if err != nil || len(b) != 0 {
		t.Fatalf("silent request: %d bytes, %v", len(b), err)
	}
	if took := time.Since(start); took > syncRequestWait+5*time.Second {
		t.Fatalf("closed after %v", took)
	}
}

// TestReaderThatStopsIsCutOff: a peer that takes a response and stops reading
// loses the stream and the slot.
func TestReaderThatStopsIsCutOff(t *testing.T) {
	ctx := t.Context()
	seed := testNode(t, ctx, Config{})
	seed.syncGate.stall = 300 * time.Millisecond
	seedReceipts(t, seed, 1)
	// More than stream windows hold, so a write must block.
	owner := newEthKey(t)
	for range 3000 {
		c, _, _ := signedClaim(t, owner, 1)
		seed.claims.Add(c)
	}
	client := testNode(t, ctx, Config{})
	connected(t, client, seed)
	s, err := client.host.NewStream(ctx, seed.host.ID(), protocol.SyncProto2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Reset()
	if _, err := s.Write([]byte(`{"since":0,"cursor_id":""}` + "\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "the slot stayed taken", func() bool {
		seed.syncGate.mu.Lock()
		defer seed.syncGate.mu.Unlock()
		return seed.syncGate.last[client.host.ID()].at != (time.Time{}) && len(seed.syncGate.active) == 0
	})
}

// scriptedSeed serves only the given protocol, with the given lines.
func scriptedSeed(t *testing.T, proto string, lines func(s network.Stream)) *quietSeed {
	t.Helper()
	q := newQuietSeed(t)
	q.host.RemoveStreamHandler(protocol.SyncProto)
	q.host.SetStreamHandler(libp2pproto.ID(proto), func(s network.Stream) {
		defer s.Close()
		lines(s)
	})
	return q
}

// TestNewClientUsesAnOldSeed: a seed that serves only the old id, in the old order,
// is synced as before: no request line, no closing line, no cursor.
func TestNewClientUsesAnOldSeed(t *testing.T) {
	ctx := t.Context()
	root, guest := newEthKey(t), newEthKey(t)
	donor := testNode(t, ctx, Config{Roots: []string{root.addr}})
	donor.members.Add(root.invite(guest.addr, 1))
	claimed, _ := claimNode(t, donor, guest)
	server := seedReceipts(t, donor, 3)

	var req []byte
	seed := scriptedSeed(t, protocol.SyncProto, func(s network.Stream) {
		s.SetReadDeadline(time.Now().Add(5 * time.Second))
		req, _ = io.ReadAll(s)
		enc := json.NewEncoder(s)
		for _, sr := range donor.receipts.All() {
			enc.Encode(sr)
		}
		for _, c := range donor.claims.All() {
			enc.Encode(map[string]any{"claim": c})
		}
		for _, it := range donor.members.Items() {
			enc.Encode(it)
		}
	})
	client := testNode(t, ctx, Config{Roots: []string{root.addr}})
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	added, err := client.syncReceipts(ctx, seed.host.ID())
	if err != nil || added != 3 {
		t.Fatalf("%d receipts, %v", added, err)
	}
	if len(req) != 0 {
		t.Fatalf("the old id got a request line: %q", req)
	}
	if client.receipts.Tally()[server].Jobs != 3 || client.accountOf(claimed) != guest.addr || !client.memberSet().Member[guest.addr] {
		t.Fatal("old seed's items did not all land")
	}
	if got := client.syncCursors.get(seed.host.ID()); got != (syncCursor{}) {
		t.Fatalf("a cursor from a seed with none: %+v", got)
	}
}

// TestOldClientGetsTheOldOrder: receipts, then claims, then invites, on the old id.
func TestOldClientGetsTheOldOrder(t *testing.T) {
	ctx := t.Context()
	root, guest := newEthKey(t), newEthKey(t)
	seed := testNode(t, ctx, Config{Roots: []string{root.addr}})
	seed.members.Add(root.invite(guest.addr, 1))
	claimNode(t, seed, guest)
	seed.syncGate.maxReceipts = 4 // the cap this test is about
	// Each server's earliest is kept past the cap: 3 servers with 1 each and one
	// with 10, oldest lines first, so a flat cap would lose them.
	var servers []string
	for range 3 {
		servers = append(servers, seedReceipts(t, seed, 1))
	}
	seedReceipts(t, seed, 10)

	client := testNode(t, ctx, Config{})
	connected(t, client, seed)
	s, err := client.host.NewStream(ctx, seed.host.ID(), protocol.SyncProto)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CloseWrite()
	s.SetDeadline(time.Now().Add(20 * time.Second))
	dec := json.NewDecoder(s)
	var kinds []string
	seen := map[string]bool{}
	for {
		var it syncItem
		if err := dec.Decode(&it); err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		switch {
		case it.Claim != nil:
			kinds = append(kinds, "claim")
		case it.Invite != nil:
			kinds = append(kinds, "invite")
		case it.Revoke != nil:
			kinds = append(kinds, "revoke")
		default:
			kinds = append(kinds, "receipt")
			seen[it.Receipt.Server] = true
		}
	}
	joined := strings.Join(kinds, " ")
	if !strings.HasSuffix(joined, "receipt claim invite") {
		t.Fatalf("order: %s", joined)
	}
	if n := strings.Count(joined, "receipt"); n != 8 { // 4 earliest + the last 4 lines, one of them not the earliest
		t.Fatalf("%d receipts, want 4 earliest and 4 recent: %s", n, joined)
	}
	for _, server := range servers {
		if !seen[server] {
			t.Fatal("a server's earliest receipt was cut")
		}
	}
}

func TestClientHoldsItsItemLimit(t *testing.T) {
	ctx := t.Context()
	seed := scriptedSeed(t, protocol.SyncProto2, func(s network.Stream) {
		s.Write([]byte(strings.Repeat(" ", syncMaxItem+2) + "\n"))
	})
	client := testNode(t, ctx, Config{})
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.syncReceipts(ctx, seed.host.ID()); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("an oversized item gave %v", err)
	}
}

func TestClientStopsOnNoProgress(t *testing.T) {
	ctx := t.Context()
	var opened atomicCounter
	seed := scriptedSeed(t, protocol.SyncProto2, func(s network.Stream) {
		opened.add()
		s.Write([]byte(`{"cursor":5,"more":true,"store_id":"x"}` + "\n"))
	})
	client := testNode(t, ctx, Config{})
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.syncReceipts(ctx, seed.host.ID()); err != nil {
		t.Fatal(err)
	}
	if got := opened.get(); got != 2 {
		t.Fatalf("%d requests to a seed that never advances", got)
	}
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) add() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestSyncRefusesFutureReceipts: a seed can deliver old history, not the future.
func TestSyncRefusesFutureReceipts(t *testing.T) {
	ctx := t.Context()
	sk, _, _ := crypto.GenerateEd25519Key(nil)
	ck, _, _ := crypto.GenerateEd25519Key(nil)
	future := signedByPair(t, ck, sk, time.Now().Add(time.Hour).Unix(), 5)
	old := signedByPair(t, ck, sk, time.Now().Add(-365*24*time.Hour).Unix(), 5)
	seed := scriptedSeed(t, protocol.SyncProto2, func(s network.Stream) {
		enc := json.NewEncoder(s)
		enc.Encode(future)
		enc.Encode(old)
		s.Write([]byte(`{"cursor":5,"store_id":"x"}` + "\n"))
	})
	client := testNode(t, ctx, Config{})
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: seed.host.ID(), Addrs: seed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if added, err := client.syncReceipts(ctx, seed.host.ID()); err != nil || added != 1 {
		t.Fatalf("%d receipts, %v", added, err)
	}
}

// TestLateInvitesCountReceiptsFromTheSameSync: the fifth invite of an account needs
// that account's "has served", which needs a receipt and a claim sent after the
// invite on sync 2.0.0. It all lands from one sync.
func TestLateInvitesCountReceiptsFromTheSameSync(t *testing.T) {
	ctx := t.Context()
	root, sharer := newEthKey(t), newEthKey(t)
	cfg := Config{Roots: []string{root.addr}}
	seed := testNode(t, ctx, cfg)
	seed.members.Add(root.invite(sharer.addr, 1))
	_, nodeKey := claimNode(t, seed, sharer)
	ck, _, _ := crypto.GenerateEd25519Key(nil)
	if !seed.receipts.Add(signedByPair(t, ck, nodeKey, time.Now().Unix(), 5)) {
		t.Fatal("receipt refused")
	}
	seed.invalidateMembers()
	var invitees []string
	for i := range members.ContributorInvites {
		guest := newEthKey(t)
		if !seed.members.Add(sharer.invite(guest.addr, int64(10+i))) {
			t.Fatal("invite refused")
		}
		invitees = append(invitees, guest.addr)
	}
	client := testNode(t, ctx, cfg)
	connected(t, client, seed)
	if _, err := client.syncReceipts(ctx, seed.host.ID()); err != nil {
		t.Fatal(err)
	}
	client.dropCachedSets()
	ms := client.memberSet()
	for _, g := range invitees {
		if !ms.Member[g] {
			t.Fatal("an invite past the first 3 was lost in the first sync")
		}
	}
}

// TestRestartKeepsTheMemberSet: compaction drops receipts, not what they mean.
func TestRestartKeepsTheMemberSet(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	root, sharer := newEthKey(t), newEthKey(t)
	dir := t.TempDir()
	cfg := Config{Roots: []string{root.addr}, Listen: []string{"/ip4/127.0.0.1/tcp/0"}, DataDir: dir, DHTServer: true,
		AnnounceEvery: 300 * time.Millisecond}
	n, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.members.Add(root.invite(sharer.addr, 1))
	_, nodeKey := claimNode(t, n, sharer)
	ck, _, _ := crypto.GenerateEd25519Key(nil)
	// All long past retention: the server's only standing is its earliest receipt.
	old := time.Now().Add(-200 * 24 * time.Hour).Unix()
	for i := range 30 {
		if !n.receipts.Add(signedByPair(t, ck, nodeKey, old+int64(i), 5)) {
			t.Fatal("receipt refused")
		}
		time.Sleep(time.Microsecond) // job ids come from the clock
	}
	n.dropCachedSets()
	// Its 4th to 10th invitees depend on it.
	for i := range members.ContributorInvites {
		if !n.members.Add(sharer.invite(newEthKey(t).addr, int64(10+i))) {
			t.Fatalf("invite %d refused", i)
		}
	}
	n.dropCachedSets()
	before := n.members.Compute()
	tally := n.receipts.Tally()
	ever := n.servedSets()
	size := fileSize(t, filepath.Join(dir, "receipts.jsonl"))
	if len(before.Member) != 2+members.ContributorInvites || !ever[sharer.addr] {
		t.Fatalf("before: %d members, ever %v", len(before.Member), ever)
	}
	n.host.Close()
	n.closeState()
	stop()

	again, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.host.Close(); again.closeState() })
	if after := fileSize(t, filepath.Join(dir, "receipts.jsonl")); after >= size {
		t.Fatalf("file not compacted: %d bytes, was %d", after, size)
	}
	after := again.members.Compute()
	if !reflect.DeepEqual(after.Member, before.Member) || !reflect.DeepEqual(after.InvitedBy, before.InvitedBy) ||
		!reflect.DeepEqual(after.Used, before.Used) || !reflect.DeepEqual(after.Sharers, before.Sharers) {
		t.Fatalf("member set changed: %d vs %d members", len(after.Member), len(before.Member))
	}
	if got := again.receipts.Tally(); !reflect.DeepEqual(got, tally) {
		t.Fatalf("tally changed: %+v vs %+v", got, tally)
	}
	if got := again.servedSets(); !reflect.DeepEqual(got, ever) {
		t.Fatalf("ever changed: %+v vs %+v", got, ever)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestFlushStateReachesTheDisk: a clean shutdown keeps what the timer had not yet
// written.
func TestFlushStateReachesTheDisk(t *testing.T) {
	ctx := t.Context()
	root, guest := newEthKey(t), newEthKey(t)
	n := testNode(t, ctx, Config{Roots: []string{root.addr}})
	n.members.Add(root.invite(guest.addr, 1))
	claimNode(t, n, guest)
	p := randomPeer(t)
	n.ledger.Served(p, 5)
	n.quota.Add("someone", 3, time.Now())
	n.flushState()
	for _, name := range []string{"members.json", "claims.json", "ledger.json", "quota.json"} {
		if fileSize(t, filepath.Join(n.cfg.DataDir, name)) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
}

// TestBadStateFilesDoNotStopANode: a file an old version or a crash left cannot
// make startup fail, since a node that exits at startup after a self-update is a
// crash loop.
func TestBadStateFilesDoNotStopANode(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"members.json", "claims.json", "ledger.json", "quota.json", "sync-cursors.json", "receipts.jsonl", "receipts.id"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{ not json\n\x00\x01"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := New(t.Context(), Config{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, DataDir: dir, DHTServer: true,
		AnnounceEvery: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("a bad state file stopped the node: %v", err)
	}
	t.Cleanup(func() { n.host.Close(); n.closeState() })
	w := httptest.NewRecorder()
	n.Gateway().ServeHTTP(w, gwReq("GET", "/credits", nil))
	if w.Code != 200 {
		t.Fatalf("/credits: %d", w.Code)
	}
}

// stalledReceiptPeer takes a receipt stream and passes it to the test.
func stalledReceiptPeer(t *testing.T, client *Node, handler func(network.Stream)) peer.ID {
	t.Helper()
	q := newQuietSeed(t)
	q.host.SetStreamHandler(protocol.ReceiptProto, handler)
	if err := client.host.Connect(t.Context(), peer.AddrInfo{ID: q.host.ID(), Addrs: q.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	return q.host.ID()
}

// TestReceiptAnswerIsBounded: a server that says nothing costs a client its
// deadline, and one that talks forever costs it a bounded read.
func TestReceiptAnswerIsBounded(t *testing.T) {
	ctx := t.Context()
	client := testNode(t, ctx, Config{})

	silent := stalledReceiptPeer(t, client, func(s network.Stream) {
		<-ctx.Done()
		s.Reset()
	})
	start := time.Now()
	if err := client.deliverReceipt(silent, receipts.SignedReceipt{}); err == nil {
		t.Fatal("a silent server gave no error")
	}
	if took := time.Since(start); took > receiptWait+5*time.Second {
		t.Fatalf("waited %v for a silent server", took)
	}

	var sent atomicCounter
	endless := stalledReceiptPeer(t, client, func(s network.Stream) {
		chunk := []byte(strings.Repeat("x", 1024))
		for range 1 << 12 {
			if _, err := s.Write(chunk); err != nil {
				return
			}
			sent.add()
		}
		<-ctx.Done()
	})
	start = time.Now()
	if err := client.deliverReceipt(endless, receipts.SignedReceipt{}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("an endless answer gave %v", err)
	}
	if took := time.Since(start); took >= receiptWait {
		t.Fatalf("read for %v instead of stopping at the cap", took)
	}
}

// TestSilentReceiptStreamIsClosed: a peer that opens a receipt stream and says
// nothing does not hold it open past the deadline.
func TestSilentReceiptStreamIsClosed(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{})
	client := testNode(t, ctx, Config{})
	connected(t, client, n)
	s, err := client.host.NewStream(ctx, n.host.ID(), protocol.ReceiptProto)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Reset()
	s.Write([]byte("{")) // start, so the stream exists on the far side, and stop
	s.SetReadDeadline(time.Now().Add(receiptWait + 10*time.Second))
	start := time.Now()
	if _, err := s.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if took := time.Since(start); took < receiptWait-time.Second {
		t.Fatalf("closed after only %v", took)
	}
}

// TestSyncAtScale: fifty thousand invites and all claims over a real stream, first
// sync and re-sync, within a bound a compute and a write per item would blow.
func TestSyncAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("signs 50k invites")
	}
	ctx := t.Context()
	root := newEthKey(t)
	const count = members.MaxInvites - 1
	keys := make([]ethKey, count)
	store.ForEach(count, func(i int) {
		k, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			panic(err)
		}
		keys[i] = ethKey{k, account.EthAddress(k.PubKey())}
	})
	items := make([]members.Item, count)
	store.ForEach(count, func(i int) {
		from := root
		if i > 0 {
			from = keys[(i-1)/members.MemberInvites]
		}
		items[i] = from.invite(keys[i].addr, int64(1000+i))
	})
	seedDir := t.TempDir()
	raw, _ := json.Marshal(items)
	if err := os.WriteFile(filepath.Join(seedDir, "members.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Roots: []string{root.addr}, Listen: []string{"/ip4/127.0.0.1/tcp/0"}, DHTServer: true,
		AnnounceEvery: 300 * time.Millisecond, DataDir: seedDir, SyncLimit: 5 * time.Minute}
	start := time.Now()
	seed, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seed.host.Close(); seed.closeState() })
	seed.syncGate.every = time.Nanosecond
	t.Logf("seed startup with %d invites: %v", count, time.Since(start))
	if got := len(seed.members.Items()); got != count {
		t.Fatalf("seed holds %d invites", got)
	}
	seedReceipts(t, seed, 200)

	client := testNode(t, ctx, Config{Roots: []string{root.addr}, SyncLimit: 5 * time.Minute})
	connected(t, client, seed)
	start = time.Now()
	if added, err := client.syncReceipts(ctx, seed.host.ID()); err != nil || added != 200 {
		t.Fatalf("first sync: %d receipts, %v", added, err)
	}
	first := time.Since(start)
	t.Logf("first sync of %d invites: %v", count, first)
	if got := len(client.members.Items()); got != count {
		t.Fatalf("client holds %d invites", got)
	}
	start = time.Now()
	if added, err := client.syncReceipts(ctx, seed.host.ID()); err != nil || added != 0 {
		t.Fatalf("re-sync: %d receipts, %v", added, err)
	}
	again := time.Since(start)
	t.Logf("re-sync: %v", again)
	if first > 3*time.Minute || again > time.Minute {
		t.Fatalf("too slow: first %v, again %v", first, again)
	}
}
