package ledger

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func testPeer(t *testing.T) peer.ID {
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := peer.IDFromPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBalanceAndReload(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	l := Open(path)
	p := testPeer(t)
	l.Received(p, 40)
	l.Served(p, 10)
	if got := l.Balance(p); got != 30 {
		t.Fatalf("balance = %d, want 30", got)
	}

	// A restart keeps the record.
	again := Open(path)
	if got := again.Balance(p); got != 30 {
		t.Fatalf("after reload: balance = %d, want 30", got)
	}
	if snap := again.Snapshot(); snap[p.String()].Gave != 40 || snap[p.String()].Took != 10 {
		t.Fatalf("snapshot: %+v", snap)
	}
}
