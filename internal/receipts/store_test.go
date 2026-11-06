package receipts

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const day = int64(24 * 3600)

type party struct {
	k  crypto.PrivKey
	id string
}

func newParty(t *testing.T) party {
	t.Helper()
	k, id := newKey(t)
	return party{k, id}
}

var jobNumber int

func receipt(t *testing.T, client, server party, ts, completion int64) SignedReceipt {
	t.Helper()
	jobNumber++
	r := Receipt{Job: fmt.Sprintf("job-%d", jobNumber), Model: "m", Client: client.id, Server: server.id,
		Prompt: 4, Completion: completion, Credits: Price(4, completion), Output: "x", TS: ts}
	return forge(t, client.k, server.k, r)
}

func openStore(t *testing.T, path string) *Receipts {
	t.Helper()
	k, _, _ := crypto.GenerateEd25519Key(nil)
	self, _ := peer.IDFromPrivateKey(k)
	rs, err := Open(path, k, self, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func storePath(t *testing.T) string { return filepath.Join(t.TempDir(), "receipts.jsonl") }

func writeLines(t *testing.T, path string, srs ...SignedReceipt) {
	t.Helper()
	var b strings.Builder
	for _, sr := range srs {
		line, _ := json.Marshal(sr)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// TestAddChecksDedupeFirst: what we already hold costs no signature check, and a
// re-sync is nearly all that.
func TestAddChecksDedupeFirst(t *testing.T) {
	rs := openStore(t, storePath(t))
	c, s := newParty(t), newParty(t)
	sr := receipt(t, c, s, time.Now().Unix(), 1)
	if !rs.Add(sr) {
		t.Fatal("refused")
	}
	before := rs.verified.Load()
	for range 100 {
		if rs.Add(sr) {
			t.Fatal("stored twice")
		}
	}
	if got := rs.verified.Load() - before; got != 0 {
		t.Fatalf("%d signature checks for receipts we hold", got)
	}
	// A new one is still checked.
	bad := receipt(t, c, s, time.Now().Unix(), 1)
	bad.ClientSig = []byte("nope")
	if rs.Add(bad) {
		t.Fatal("bad signature stored")
	}
}

func TestOpenSkipsWhatItCannotUse(t *testing.T) {
	path := storePath(t)
	c, s := newParty(t), newParty(t)
	now := time.Now().Unix()
	good1, good2 := receipt(t, c, s, now, 1), receipt(t, c, s, now, 2)
	forged := receipt(t, c, s, now, 3)
	forged.Receipt.Credits = 1 << 20
	l1, _ := json.Marshal(good1)
	l2, _ := json.Marshal(good2)
	lf, _ := json.Marshal(forged)
	content := string(l1) + "\n" +
		"not json\n" +
		"\n" +
		`{"total":{"credits":3}}` + "\n" + // a total for no server
		string(lf) + "\n" +
		string(l1) + "\n" + // a duplicate
		strings.Repeat("x", maxLine+10) + "\n" + // longer than any line we would write
		string(l2) + "\n" +
		`{"receipt":{"job":"cut sho` // what a crash leaves
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rs := openStore(t, path)
	if got := rs.Tally()[s.id]; got.Jobs != 2 || got.Credits != good1.Receipt.Credits+good2.Receipt.Credits {
		t.Fatalf("tally %+v", got)
	}
	if len(rs.All()) != 2 {
		t.Fatalf("%d receipts", len(rs.All()))
	}
	// The next append does not join the line the crash cut short.
	next := receipt(t, c, s, now, 4)
	if !rs.Add(next) {
		t.Fatal("refused")
	}
	if got := openStore(t, path).Tally()[s.id]; got.Jobs != 3 {
		t.Fatalf("after reload: %+v", got)
	}
}

func TestLongValidLineSurvivesTheScanner(t *testing.T) {
	path := storePath(t)
	c, s := newParty(t), newParty(t)
	r := Receipt{Job: "long", Model: "m", Client: c.id, Server: s.id, Prompt: 4, Completion: 1, Credits: Price(4, 1),
		Output: strings.Repeat("a", 200<<10), TS: time.Now().Unix()}
	writeLines(t, path, forge(t, c.k, s.k, r), receipt(t, c, s, time.Now().Unix(), 1))
	if got := openStore(t, path).Tally()[s.id].Jobs; got != 2 {
		t.Fatalf("%d jobs", got)
	}
}

func TestNewOversizedReceiptIsRefused(t *testing.T) {
	rs := openStore(t, storePath(t))
	c, s := newParty(t), newParty(t)
	r := Receipt{Job: "fat", Model: "m", Client: c.id, Server: s.id, Prompt: 4, Completion: 1, Credits: Price(4, 1),
		Output: strings.Repeat("a", maxRecord), TS: time.Now().Unix()}
	if rs.Add(forge(t, c.k, s.k, r)) {
		t.Fatal("stored a receipt over the record limit")
	}
}

func TestCountersMatchTheReceipts(t *testing.T) {
	rs := openStore(t, storePath(t))
	c, a, b := newParty(t), newParty(t), newParty(t)
	now := time.Now().Unix()
	// Out of time order, as signers choose TS.
	for _, x := range []struct {
		s          party
		ts, tokens int64
	}{{a, now - 5*day, 10}, {a, now - 1*day, 1}, {a, now - 3*day, 100}, {b, now - 8*day, 7}} {
		if !rs.Add(receipt(t, c, x.s, x.ts, x.tokens)) {
			t.Fatal("refused")
		}
	}
	if got := rs.Tally(); got[a.id].Jobs != 3 || got[a.id].Credits != 3*Price(4, 0)+111 || got[b.id].Jobs != 1 {
		t.Fatalf("tally %+v", got)
	}
	since := now - 7*day
	if got := rs.ServersSince(since); !got[a.id] || got[b.id] || len(got) != 2 {
		t.Fatalf("servers since: %+v", got)
	}
	if got := rs.JobsSince(now - 4*day); got[a.id] != 2 || got[b.id] != 0 {
		t.Fatalf("jobs since: %+v", got)
	}
	if got := rs.JobsSince(now - 5*day); got[a.id] != 3 {
		t.Fatalf("jobs since, at the boundary: %+v", got)
	}
}

// ageStore makes a store with 40 dead receipts and 3 servers. A's earliest and B's
// only receipt are past retention; C has only fresh ones.
func ageStore(t *testing.T, path string) (a, b, c party) {
	t.Helper()
	cl := newParty(t)
	a, b, c = newParty(t), newParty(t), newParty(t)
	now := time.Now().Unix()
	old := now - 100*day
	var srs []SignedReceipt
	for i := range 40 {
		srs = append(srs, receipt(t, cl, a, old+int64(i), 10))
	}
	srs = append(srs, receipt(t, cl, b, old-5, 7))
	for i := range 10 {
		srs = append(srs, receipt(t, cl, a, now-int64(i), 3))
		srs = append(srs, receipt(t, cl, c, now-int64(i), 5))
	}
	writeLines(t, path, srs...)
	return a, b, c
}

func TestCompactionKeepsEveryAnswer(t *testing.T) {
	path := storePath(t)
	a, b, c := ageStore(t, path)
	raw, _ := os.ReadFile(path)
	now := time.Now().Unix()
	before := lineCount(t, path)
	wantTally := sumFile(t, raw)
	since := now - 7*day

	rs := openStore(t, path)
	after := lineCount(t, path)
	if after >= before {
		t.Fatalf("not compacted: %d lines, was %d", after, before)
	}
	if got := rs.Tally(); !reflect.DeepEqual(got, wantTally) {
		t.Fatalf("tally changed:\n got %+v\nwant %+v", got, wantTally)
	}
	check := func(r *Receipts) {
		t.Helper()
		if got := r.Tally(); !reflect.DeepEqual(got, wantTally) {
			t.Fatalf("tally changed:\n got %+v\nwant %+v", got, wantTally)
		}
		ever := r.ServersSince(since)
		if len(ever) != 3 || ever[a.id] != true || ever[b.id] != false || ever[c.id] != true {
			t.Fatalf("ever: %+v", ever)
		}
		if got := r.JobsSince(since); got[a.id] != 10 || got[c.id] != 10 || got[b.id] != 0 {
			t.Fatalf("jobs: %+v", got)
		}
	}
	check(rs)
	// Every server still has a receipt behind it. Only the fresh ones and the
	// earliest of each server are left.
	kept := map[string]int{}
	for _, sr := range rs.All() {
		kept[sr.Receipt.Server]++
	}
	if kept[a.id] != 11 || kept[b.id] != 1 || kept[c.id] != 10 {
		t.Fatalf("kept %+v", kept)
	}
	// Reopening changes nothing and does not compact again.
	id := rs.StoreID()
	again := openStore(t, path)
	check(again)
	if again.StoreID() != id {
		t.Fatal("a second open bumped the store id")
	}
}

// sumFile sums a receipts file directly, for tests to compare with.
func sumFile(t *testing.T, raw []byte) map[string]ServerCredits {
	t.Helper()
	out := map[string]ServerCredits{}
	for _, line := range strings.Split(string(raw), "\n") {
		var sr SignedReceipt
		if json.Unmarshal([]byte(line), &sr) == nil && sr.Receipt.Job != "" {
			c := out[sr.Receipt.Server]
			c.Credits += sr.Receipt.Credits
			c.Jobs++
			out[sr.Receipt.Server] = c
		}
	}
	return out
}

func TestCompactionLeavesAFewDeadRecordsAlone(t *testing.T) {
	path := storePath(t)
	cl, s := newParty(t), newParty(t)
	now := time.Now().Unix()
	srs := []SignedReceipt{receipt(t, cl, s, now-100*day, 1), receipt(t, cl, s, now-99*day, 1)}
	for range 20 {
		srs = append(srs, receipt(t, cl, s, now, 1))
	}
	writeLines(t, path, srs...)
	before := lineCount(t, path)
	rs := openStore(t, path)
	if got := lineCount(t, path); got != before {
		t.Fatalf("compacted at under 20%% dead: %d lines, was %d", got, before)
	}
	// The dead one is no longer served, though it is still on disk.
	if got := len(rs.All()); got != 21 {
		t.Fatalf("%d receipts served, want the earliest and 20 fresh", got)
	}
}

// After compaction the totals include receipts we no longer hold, so hearing those
// again from a seed must not count them twice.
func TestReheardHistoryIsNotCountedTwice(t *testing.T) {
	path := storePath(t)
	cl, s := newParty(t), newParty(t)
	now := time.Now().Unix()
	var history []SignedReceipt
	for i := range 20 {
		history = append(history, receipt(t, cl, s, now-100*day+int64(i), 10))
	}
	writeLines(t, path, history...)
	rs := openStore(t, path)
	want := rs.Tally()[s.id]
	if want.Jobs != 20 {
		t.Fatalf("jobs %d", want.Jobs)
	}
	for _, sr := range history {
		if rs.AddSynced(sr) {
			t.Fatalf("job %s counted again", sr.Receipt.Job)
		}
	}
	if got := rs.Tally()[s.id]; got != want {
		t.Fatalf("tally %+v, want %+v", got, want)
	}
	// An even earlier receipt is news: it becomes the server's earliest.
	first := receipt(t, cl, s, now-200*day, 10)
	if !rs.AddSynced(first) {
		t.Fatal("new earliest refused")
	}
	reopened := openStore(t, path)
	if got := reopened.Tally()[s.id]; got.Jobs != 21 {
		t.Fatalf("after reopen: %+v", got)
	}
}

// An old seed serves its whole history, older than retention included. It all
// counts the first time, in any order, once.
func TestFullHistoryFromAnOldSeedCountsOnce(t *testing.T) {
	path := storePath(t)
	rs := openStore(t, path)
	cl, s := newParty(t), newParty(t)
	now := time.Now().Unix()
	var history []SignedReceipt
	for _, age := range []int64{100, 120, 110, 95, 130} {
		history = append(history, receipt(t, cl, s, now-age*day, 10))
	}
	for _, sr := range history {
		if !rs.AddSynced(sr) {
			t.Fatal("refused history")
		}
	}
	want := rs.Tally()[s.id]
	if want.Jobs != 5 {
		t.Fatalf("jobs %d", want.Jobs)
	}
	for _, sr := range history {
		if rs.AddSynced(sr) {
			t.Fatal("counted twice in one run")
		}
	}
	reopened := openStore(t, path)
	if got := reopened.Tally()[s.id]; got != want {
		t.Fatalf("after restart: %+v, want %+v", got, want)
	}
	for _, sr := range history {
		if reopened.AddSynced(sr) {
			t.Fatal("counted twice across a restart")
		}
	}
}

func TestSyncRefusesFutureReceipts(t *testing.T) {
	rs := openStore(t, storePath(t))
	cl, s := newParty(t), newParty(t)
	now := time.Now()
	if rs.AddSynced(receipt(t, cl, s, now.Add(11*time.Minute).Unix(), 1)) {
		t.Fatal("a receipt dated past the skew was stored")
	}
	if !rs.AddSynced(receipt(t, cl, s, now.Add(5*time.Minute).Unix(), 1)) {
		t.Fatal("a receipt within the skew was refused")
	}
	if !rs.AddSynced(receipt(t, cl, s, now.Add(-400*24*time.Hour).Unix(), 1)) {
		t.Fatal("an old receipt was refused")
	}
}

func TestServerTableIsBounded(t *testing.T) {
	rs := openStore(t, storePath(t))
	cl, known := newParty(t), newParty(t)
	now := time.Now().Unix()
	rs.Add(receipt(t, cl, known, now, 1))
	for i := range maxServers {
		rs.servers[fmt.Sprintf("filler-%d", i)] = &serverStats{}
	}
	if rs.Add(receipt(t, cl, newParty(t), now, 1)) {
		t.Fatal("a new server got in past the cap")
	}
	if !rs.Add(receipt(t, cl, known, now, 1)) {
		t.Fatal("a known server was refused")
	}
}

func TestStoredSetIsBounded(t *testing.T) {
	rs := openStore(t, storePath(t))
	cl, known := newParty(t), newParty(t)
	now := time.Now().Unix()
	rs.Add(receipt(t, cl, known, now, 1))
	for i := range maxStored {
		rs.seen[jobKey{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}] = struct{}{}
	}
	if rs.Add(receipt(t, cl, known, now, 1)) {
		t.Fatal("stored past the cap")
	}
	// A server's new earliest and a new server still get in, each with its own bound.
	if !rs.Add(receipt(t, cl, known, now-10, 1)) || !rs.Add(receipt(t, cl, newParty(t), now, 1)) {
		t.Fatal("the record behind has-ever-served was refused")
	}
}

func emitInto(out *[]SignedReceipt) func([]byte) error {
	return func(line []byte) error {
		var sr SignedReceipt
		if err := json.Unmarshal(line, &sr); err != nil {
			return err
		}
		*out = append(*out, sr)
		return nil
	}
}

func TestStreamFollowsACursor(t *testing.T) {
	rs := openStore(t, storePath(t))
	cl, s := newParty(t), newParty(t)
	now := time.Now().Unix()
	for i := range 5 {
		rs.Add(receipt(t, cl, s, now-int64(i), 1))
	}
	id := rs.StoreID()
	var got []SignedReceipt
	cur, more, err := rs.Stream(0, "", 3, math.MaxInt64, emitInto(&got))
	if err != nil || !more || len(got) != 3 {
		t.Fatalf("first: %d receipts, more=%v, %v", len(got), more, err)
	}
	cur, more, err = rs.Stream(cur, id, 3, math.MaxInt64, emitInto(&got))
	if err != nil || more || len(got) != 5 {
		t.Fatalf("second: %d receipts, more=%v, %v", len(got), more, err)
	}
	seen := map[string]bool{}
	for _, sr := range got {
		if seen[sr.Receipt.Job] {
			t.Fatal("a receipt came twice")
		}
		seen[sr.Receipt.Job] = true
	}
	// Nothing new: nothing sent, and the cursor holds.
	var none []SignedReceipt
	if next, more, _ := rs.Stream(cur, id, 3, math.MaxInt64, emitInto(&none)); len(none) != 0 || more || next != cur {
		t.Fatalf("idle: %d receipts, more=%v, cursor %d->%d", len(none), more, cur, next)
	}
	// A new one arrives, and only it is sent, even with a TS older than the rest:
	// the cursor follows arrival, which no signer can choose.
	late := receipt(t, cl, s, now-50*day, 1)
	if !rs.Add(late) {
		t.Fatal("refused")
	}
	var fresh []SignedReceipt
	rs.Stream(cur, id, 3, math.MaxInt64, emitInto(&fresh))
	if len(fresh) != 1 || fresh[0].Receipt.Job != late.Receipt.Job {
		t.Fatalf("after one more: %+v", fresh)
	}
}

func TestStreamStartsOverOnAStrangeCursor(t *testing.T) {
	rs := openStore(t, storePath(t))
	cl, s := newParty(t), newParty(t)
	for range 3 {
		rs.Add(receipt(t, cl, s, time.Now().Unix(), 1))
	}
	for name, c := range map[string]struct {
		since int64
		id    string
	}{"another store": {100, "other"}, "past the end": {1 << 40, rs.StoreID()}, "negative": {-1, rs.StoreID()}} {
		var got []SignedReceipt
		if _, _, err := rs.Stream(c.since, c.id, 100, math.MaxInt64, emitInto(&got)); err != nil || len(got) != 3 {
			t.Fatalf("%s: %d receipts, %v", name, len(got), err)
		}
	}
	// A cursor in the middle of a line loses nothing but that line's bytes.
	var got []SignedReceipt
	if _, _, err := rs.Stream(5, rs.StoreID(), 100, math.MaxInt64, func(line []byte) error {
		return emitInto(&got)(line)
	}); err != nil || len(got) != 2 {
		t.Fatalf("mid-line: %d receipts, %v", len(got), err)
	}
}

func TestStreamHoldsItsBudget(t *testing.T) {
	rs := openStore(t, storePath(t))
	cl, s := newParty(t), newParty(t)
	for range 4 {
		rs.Add(receipt(t, cl, s, time.Now().Unix(), 1))
	}
	var one []byte
	rs.Stream(0, "", 1, math.MaxInt64, func(line []byte) error { one = append([]byte(nil), line...); return nil })
	budget := int64(len(one))*2 + 1
	var got []SignedReceipt
	cur, more, err := rs.Stream(0, "", 100, budget, emitInto(&got))
	if err != nil || !more || len(got) != 2 {
		t.Fatalf("%d receipts, more=%v, %v", len(got), more, err)
	}
	// It continues where the budget stopped it.
	rs.Stream(cur, rs.StoreID(), 100, math.MaxInt64, emitInto(&got))
	if len(got) != 4 {
		t.Fatalf("%d receipts in all", len(got))
	}
}

func TestStreamSkipsDeadButKeepsEarliest(t *testing.T) {
	path := storePath(t)
	cl, s := newParty(t), newParty(t)
	now := time.Now().Unix()
	first := receipt(t, cl, s, now-200*day, 1)
	dead := receipt(t, cl, s, now-150*day, 1)
	live := receipt(t, cl, s, now, 1)
	var srs []SignedReceipt
	srs = append(srs, dead, first, live)
	for range 20 {
		srs = append(srs, receipt(t, cl, s, now, 1))
	}
	writeLines(t, path, srs...)
	rs := openStore(t, path)
	got := map[string]bool{}
	for _, sr := range rs.All() {
		got[sr.Receipt.Job] = true
	}
	if !got[first.Receipt.Job] || got[dead.Receipt.Job] || !got[live.Receipt.Job] {
		t.Fatalf("served %v", got)
	}
}

func TestStreamRecentKeepsEarliestPlusTail(t *testing.T) {
	path := storePath(t)
	cl := newParty(t)
	a, b := newParty(t), newParty(t)
	now := time.Now().Unix()
	first := receipt(t, cl, a, now-200*day, 1)
	only := receipt(t, cl, b, now-300*day, 1)
	var srs []SignedReceipt
	srs = append(srs, first, only)
	for i := range 40 {
		srs = append(srs, receipt(t, cl, a, now-int64(i), 1))
	}
	writeLines(t, path, srs...)
	rs := openStore(t, path)

	var got []SignedReceipt
	if err := rs.StreamRecent(10, math.MaxInt64, emitInto(&got)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 {
		t.Fatalf("%d receipts, want 2 earliest and 10 recent", len(got))
	}
	jobs := map[string]bool{}
	for _, sr := range got {
		if jobs[sr.Receipt.Job] {
			t.Fatal("a receipt came twice")
		}
		jobs[sr.Receipt.Job] = true
	}
	if !jobs[first.Receipt.Job] || !jobs[only.Receipt.Job] {
		t.Fatal("an earliest receipt was left out")
	}
	if !jobs[srs[len(srs)-1].Receipt.Job] || jobs[srs[2].Receipt.Job] {
		t.Fatal("the tail is not the most recent lines")
	}

	// A small budget keeps the earliest ones at least.
	var one []byte
	rs.StreamRecent(0, math.MaxInt64, func(line []byte) error { one = append([]byte(nil), line...); return nil })
	got = nil
	if err := rs.StreamRecent(10, int64(len(one))*3, emitInto(&got)); err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || len(got) > 3 {
		t.Fatalf("%d receipts under the budget", len(got))
	}
	// Nothing fits: nothing is sent, and it is not an error.
	got = nil
	if err := rs.StreamRecent(10, 1, emitInto(&got)); err != nil || len(got) != 0 {
		t.Fatalf("%d receipts, %v", len(got), err)
	}
}

func TestTailOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	content := strings.Repeat("0123456789\n", 100_000)
	os.WriteFile(path, []byte(content), 0o600)
	f, _ := os.Open(path)
	defer f.Close()
	size := int64(len(content))
	for n, want := range map[int]int64{1: size - 11, 2: size - 22, 50_000: size - 550_000, 100_000: 0, 200_000: 0} {
		got, err := tailOffset(f, size, n)
		if err != nil || got != want {
			t.Fatalf("n=%d: got %d, want %d (%v)", n, got, want, err)
		}
	}
}

func TestStoreIDPersistsAndCompactionChangesIt(t *testing.T) {
	path := storePath(t)
	rs := openStore(t, path)
	id := rs.StoreID()
	if len(id) != 32 {
		t.Fatalf("id %q", id)
	}
	if again := openStore(t, path); again.StoreID() != id {
		t.Fatal("store id changed across a plain restart")
	}
	ageStore(t, path)
	if openStore(t, path).StoreID() == id {
		t.Fatal("compaction kept the store id, and every cursor with it")
	}
	// An unreadable id is replaced, not fatal.
	os.WriteFile(idPath(path), []byte("junk"), 0o600)
	if got := openStore(t, path).StoreID(); len(got) != 32 {
		t.Fatalf("id %q", got)
	}
}

func TestCompactionCursorStartsOver(t *testing.T) {
	path := storePath(t)
	ageStore(t, path)
	first := openStore(t, path) // compacts
	id := first.StoreID()
	var got []SignedReceipt
	cur, _, err := first.Stream(0, "", 1000, math.MaxInt64, emitInto(&got))
	if err != nil {
		t.Fatal(err)
	}
	// A later run has more dead lines to drop, so positions move.
	cl, s := newParty(t), newParty(t)
	var extra []SignedReceipt
	for range 40 {
		extra = append(extra, receipt(t, cl, s, time.Now().Unix()-100*day, 1))
	}
	for _, sr := range extra {
		line, _ := json.Marshal(sr)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		f.Write(append(line, '\n'))
		f.Close()
	}
	second := openStore(t, path)
	if second.StoreID() == id {
		t.Fatal("no new id after compaction")
	}
	var again []SignedReceipt
	if _, _, err := second.Stream(cur, id, 1000, math.MaxInt64, emitInto(&again)); err != nil {
		t.Fatal(err)
	}
	if len(again) < len(got) {
		t.Fatalf("an old cursor skipped receipts: %d, want at least %d", len(again), len(got))
	}
}

// isOurs must recognise our key and nothing else.
func TestProvenNeedsOurSeed(t *testing.T) {
	seed := make([]byte, 32)
	seed[0] = 9
	k, _, _ := crypto.GenerateEd25519Key(nil)
	self, _ := peer.IDFromPrivateKey(k)
	path := storePath(t)
	rs, err := Open(path, k, self, seed)
	if err != nil {
		t.Fatal(err)
	}
	s := newParty(t)
	sid, _ := peer.Decode(s.id)
	now := time.Now()
	ours := func(at time.Time, keyAt time.Time, tokens int64) SignedReceipt {
		ck, err := Key(seed, sid, keyAt)
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := peer.IDFromPrivateKey(ck)
		return receipt(t, party{ck, cid.String()}, s, at.Unix(), tokens)
	}
	since := now.Add(-7 * 24 * time.Hour).Unix()

	// A stranger's two keys: valid, and proves nothing.
	if !rs.Add(receipt(t, newParty(t), s, now.Unix(), 10)) {
		t.Fatal("refused")
	}
	if len(rs.Proven(since)) != 0 {
		t.Fatal("a stranger's receipt counted as proof")
	}
	// Ours with no credits proves nothing either.
	zero := ours(now, now, 0)
	zero.Receipt.Prompt, zero.Receipt.Credits = 0, 0
	ck, _ := Key(seed, sid, now)
	cid, _ := peer.IDFromPrivateKey(ck)
	zero = forge(t, ck, s.k, zero.Receipt)
	if zero.Receipt.Client != cid.String() || !rs.Add(zero) || len(rs.Proven(since)) != 0 {
		t.Fatal("a zero-credit receipt counted as proof")
	}
	// A job we asked for does.
	if !rs.Add(ours(now, now, 10)) || !rs.Proven(since)[s.id] {
		t.Fatal("our own receipt did not count")
	}
	// Survives a restart, since it is read back from the file.
	again, err := Open(path, k, self, seed)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Proven(since)[s.id] {
		t.Fatal("lost across a restart")
	}
	// Without the seed nothing is proof.
	blind, err := Open(path, k, self, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(blind.Proven(since)) != 0 {
		t.Fatal("proof without a seed")
	}
	// A key from the epoch before the TS counts, for a job straddling the turn.
	other := newParty(t)
	oid, _ := peer.Decode(other.id)
	ckOld, _ := Key(seed, oid, now.Add(-PseudonymEpoch))
	oldID, _ := peer.IDFromPrivateKey(ckOld)
	if !rs.Add(receipt(t, party{ckOld, oldID.String()}, other, now.Unix(), 10)) || !rs.Proven(since)[other.id] {
		t.Fatal("the previous epoch's key did not count")
	}
	// Two epochs back does not.
	third := newParty(t)
	tid, _ := peer.Decode(third.id)
	ckOlder, _ := Key(seed, tid, now.Add(-2*PseudonymEpoch))
	olderID, _ := peer.IDFromPrivateKey(ckOlder)
	if !rs.Add(receipt(t, party{ckOlder, olderID.String()}, third, now.Unix(), 10)) || rs.Proven(since)[third.id] {
		t.Fatal("a key from two epochs back counted")
	}
	// Expired: outside the window asked about.
	fourth := newParty(t)
	fid, _ := peer.Decode(fourth.id)
	at := now.Add(-7*24*time.Hour - time.Hour)
	ckAged, _ := Key(seed, fid, at)
	agedID, _ := peer.IDFromPrivateKey(ckAged)
	if !rs.Add(receipt(t, party{ckAged, agedID.String()}, fourth, at.Unix(), 10)) || rs.Proven(since)[fourth.id] {
		t.Fatal("an old job still counted")
	}
}

func TestPseudonymCacheIsBounded(t *testing.T) {
	seed := make([]byte, 32)
	rs := &Receipts{seed: seed}
	for range maxMine + 5 {
		if rs.pseudonym(newParty(t).id, time.Now().Unix()) == "" {
			t.Fatal("no pseudonym")
		}
		if len(rs.mine) > maxMine {
			t.Fatalf("cache at %d", len(rs.mine))
		}
	}
	if rs.pseudonym("not a peer id", 1) != "" {
		t.Fatal("a bad server id made a pseudonym")
	}
}

func TestLoadSeedWritesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed")
	a, err := LoadSeed(path)
	if err != nil || len(a) != 32 {
		t.Fatalf("%x %v", a, err)
	}
	b, _ := LoadSeed(path)
	if string(a) != string(b) {
		t.Fatal("seed changed")
	}
}

func TestServedAndCountersign(t *testing.T) {
	sk, sid := newKey(t)
	server, _ := peer.Decode(sid)
	rs, err := Open(storePath(t), sk, server, nil)
	if err != nil {
		t.Fatal(err)
	}
	ck, cid := newKey(t)
	from, _ := peer.Decode(cid)
	r := Receipt{Job: "j", Model: "m", Client: cid, Server: sid, Prompt: 4, Completion: 1, Credits: 2, Output: "o", TS: time.Now().Unix()}
	sign := func(r Receipt) *SignedReceipt {
		s, _ := ck.Sign(r.Bytes())
		return &SignedReceipt{Receipt: r, ClientSig: s}
	}
	if err := rs.Countersign(sign(r), from); err != ErrUnknownJob {
		t.Fatalf("unknown job: %v", err)
	}
	rs.Served("j", from, "m", 4, 1, "o", 2)
	bad := r
	bad.Completion = 9
	if err := rs.Countersign(sign(bad), from); err == nil {
		t.Fatal("countersigned a receipt that does not match")
	}
	rs.Served("j", from, "m", 4, 1, "o", 2)
	sr := sign(r)
	if err := rs.Countersign(sr, from); err != nil {
		t.Fatal(err)
	}
	if !sr.Valid() || !rs.Add(*sr) {
		t.Fatal("countersigned receipt not valid")
	}
	rs.Served("k", from, "m", 4, 1, "o", 2)
	wrong := sign(Receipt{Job: "k", Model: "m", Client: cid, Server: sid, Prompt: 4, Completion: 1, Credits: 2, Output: "o", TS: 1})
	wrong.ClientSig = []byte("nope")
	if err := rs.Countersign(wrong, from); err == nil {
		t.Fatal("countersigned a bad client signature")
	}
	if err := rs.Flush(); err != nil {
		t.Fatal(err)
	}
}
