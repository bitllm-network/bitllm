package ledger

import (
	"fmt"
	"os"
	"strings"
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
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	again := Open(path)
	if got := again.Balance(p); got != 30 {
		t.Fatalf("after reload: balance = %d, want 30", got)
	}
	if snap := again.Snapshot(); snap[p.String()].Gave != 40 || snap[p.String()].Took != 10 {
		t.Fatalf("snapshot: %+v", snap)
	}
}

func TestPeerCapEvictsLeastActive(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	l := Open(path)
	l.maxPeers = 3
	small, mid, big := testPeer(t), testPeer(t), testPeer(t)
	l.Served(small, 1)
	l.Received(mid, 5)
	l.Served(big, 100)

	// A newcomer with more activity pushes out the smallest.
	busy := testPeer(t)
	l.Received(busy, 10)
	snap := l.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("%d peers, want 3", len(snap))
	}
	if _, ok := snap[small.String()]; ok {
		t.Fatal("the least active peer stayed")
	}
	// One with less than everyone here does not get in.
	if l.Served(testPeer(t), 1); len(l.Snapshot()) != 3 {
		t.Fatal("cap not held")
	}
	if _, ok := l.Snapshot()[mid.String()]; !ok {
		t.Fatal("a small newcomer pushed out a larger peer")
	}
}

func TestDefaultPeerCap(t *testing.T) {
	if MaxPeers != 10000 || Open(t.TempDir()+"/l").maxPeers != MaxPeers {
		t.Fatal("cap is not 10000")
	}
}

func TestOversizedFileIsTrimmedOnOpen(t *testing.T) {
	path := t.TempDir() + "/ledger.json"
	var b strings.Builder
	b.WriteString(`{"peers":{`)
	for i := range MaxPeers + 5 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"p%d":{"gave":%d,"took":0}`, i, i)
	}
	b.WriteString(`}}`)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Open(path)
	snap := l.Snapshot()
	if len(snap) != MaxPeers {
		t.Fatalf("%d peers, want %d", len(snap), MaxPeers)
	}
	if _, ok := snap["p0"]; ok {
		t.Fatal("the least active entry survived")
	}
}

func TestBalanceDoesNotCreateAnEntry(t *testing.T) {
	l := Open(t.TempDir() + "/ledger.json")
	if l.Balance(testPeer(t)) != 0 || len(l.Snapshot()) != 0 {
		t.Fatal("reading a balance stored a peer")
	}
}

func TestBadFilesDoNotStopTheNode(t *testing.T) {
	for _, content := range []string{"not json", `{"peers":{"x":null}}`, `{"peers":null}`} {
		path := t.TempDir() + "/ledger.json"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		l := Open(path)
		if len(l.Snapshot()) != 0 {
			t.Fatalf("%q: %+v", content, l.Snapshot())
		}
	}
}

func TestManyChangesAreOneWrite(t *testing.T) {
	l := Open(t.TempDir() + "/ledger.json")
	p := testPeer(t)
	for range 1000 {
		l.Served(p, 1)
	}
	if got := l.save.Writes(); got != 0 {
		t.Fatalf("%d writes before the flush", got)
	}
	if err := l.Flush(); err != nil || l.save.Writes() != 1 {
		t.Fatalf("flush: %v, %d writes", err, l.save.Writes())
	}
}
