package receipts

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func newKey(t *testing.T) (crypto.PrivKey, string) {
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := peer.IDFromPrivateKey(k)
	return k, id.String()
}

// forge makes a receipt signed by both parties, as two colluding keys could.
func forge(t *testing.T, ck, sk crypto.PrivKey, r Receipt) SignedReceipt {
	cs, _ := ck.Sign(r.Bytes())
	ss, _ := sk.Sign(r.Bytes())
	return SignedReceipt{Receipt: r, ClientSig: cs, ServerSig: ss}
}

func TestReceiptCreditsFollowTokens(t *testing.T) {
	ck, cid := newKey(t)
	sk, sid := newKey(t)
	r := Receipt{Job: "j1", Model: "m", Client: cid, Server: sid, Prompt: 40, Completion: 10, Credits: 20, TS: time.Now().Unix()}
	if sr := forge(t, ck, sk, r); !sr.Valid() {
		t.Fatal("honest receipt rejected")
	}
	r.Credits = 1e18
	if sr := forge(t, ck, sk, r); sr.Valid() {
		t.Fatal("credits not derived from tokens, yet accepted")
	}
	r.Completion, r.Credits = 1e15, Price(40, 1e15)
	if sr := forge(t, ck, sk, r); sr.Valid() {
		t.Fatal("absurd token count accepted")
	}
}

func TestReceiptGossipLimits(t *testing.T) {
	ck, cid := newKey(t)
	sk, sid := newKey(t)
	rs := &Receipts{}
	server, _ := peer.Decode(sid)
	other, _ := peer.Decode(cid)
	mk := func(job string, ts int64) *SignedReceipt {
		sr := forge(t, ck, sk, Receipt{Job: job, Model: "m", Client: cid, Server: sid, Prompt: 4, Completion: 1, Credits: 2, TS: ts})
		return &sr
	}
	now := time.Now().Unix()
	if rs.AcceptGossip(other, mk("a", now)) {
		t.Fatal("receipt relayed under another origin accepted")
	}
	if rs.AcceptGossip(server, mk("b", now-2*3600)) {
		t.Fatal("stale receipt accepted")
	}
	ok := 0
	for i := range receiptPerServer + 50 {
		if rs.AcceptGossip(server, mk(string(rune('A'+i%26))+time.Now().String(), now)) {
			ok++
		}
	}
	// The minute may roll over mid-loop; allow one extra window.
	if ok < receiptPerServer || ok > 2*receiptPerServer {
		t.Fatalf("per-server limit let %d through", ok)
	}
}

// TestReceiptPseudonym: one stable key per server per 30-day epoch, different
// across servers and epochs, and unrelated to the node key.
func TestReceiptPseudonym(t *testing.T) {
	seed := make([]byte, 32)
	seed[0] = 1
	k1, _, _ := crypto.GenerateEd25519Key(nil)
	k2, _, _ := crypto.GenerateEd25519Key(nil)
	s1, _ := peer.IDFromPrivateKey(k1)
	s2, _ := peer.IDFromPrivateKey(k2)
	id := func(server peer.ID, at time.Time) peer.ID {
		k, err := Key(seed, server, at)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := peer.IDFromPrivateKey(k)
		return p
	}
	now := time.Unix(1_800_000_000, 0)
	if id(s1, now) != id(s1, now.Add(time.Hour)) {
		t.Fatal("pseudonym changed within an epoch")
	}
	if id(s1, now) == id(s2, now) {
		t.Fatal("same pseudonym for two servers")
	}
	if id(s1, now) == id(s1, now.Add(PseudonymEpoch)) {
		t.Fatal("pseudonym did not rotate across epochs")
	}
	other := append([]byte(nil), seed...)
	other[0] = 2
	k, _ := Key(other, s1, now)
	if p, _ := peer.IDFromPrivateKey(k); p == id(s1, now) {
		t.Fatal("a new seed kept the old pseudonym")
	}
}
