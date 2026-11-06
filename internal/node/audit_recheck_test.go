package node

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// recheckAudits is a client's audit table with a scripted checker: each check
// reports bad, and the test counts the calls. There is one auditor, so bad
// results alone keep a peer a suspect and never make it fail.
func recheckAudits(bad *atomic.Bool, calls *atomic.Int32) *audits {
	a := &audits{every: 50, peers: map[auditKey]*peerAudit{}, banned: map[peer.ID]time.Time{}}
	a.checkFn = func(peer.ID, string, reqParams, map[peer.ID]bool) (auditCheck, bool) {
		calls.Add(1)
		return auditCheck{Bad: bad.Load(), Stats: &auditStats{}, Auditor: "auditor"}, true
	}
	return a
}

// roundDone waits for a round started by sweep or observe to report back.
func roundDone(t *testing.T, a *audits, k auditKey) {
	t.Helper()
	eventually(t, 5*time.Second, "the round did not finish", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.peers[k].running
	})
}

// A suspect that no real request reaches is checked again by the timer, and a
// good answer clears it, so it goes back to taking traffic.
func TestAuditTimerClearsASuspect(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	a := recheckAudits(&bad, &calls)
	k := auditKey{peer.ID("srv"), "m"}
	now := time.Now()
	a.peers[k] = &peerAudit{State: "suspect", lastBad: now.Add(-2 * time.Hour)}
	if _, demoted := a.rank(k.p, k.model); !demoted {
		t.Fatal("a suspect is not demoted")
	}
	a.sweep(now)
	roundDone(t, a, k)
	if calls.Load() != 1 || a.peers[k].State != "pass" {
		t.Fatalf("%d checks, state %s", calls.Load(), a.peers[k].State)
	}
	if _, demoted := a.rank(k.p, k.model); demoted {
		t.Fatal("still demoted after a good check")
	}
}

// The timer starts at most one round an hour for one suspect.
func TestAuditTimerRechecksAtMostHourly(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	bad.Store(true)
	a := recheckAudits(&bad, &calls)
	k := auditKey{peer.ID("srv"), "m"}
	start := time.Now()
	a.peers[k] = &peerAudit{State: "suspect", lastBad: start}
	a.sweep(start)
	roundDone(t, a, k)
	first := calls.Load()
	if first == 0 {
		t.Fatal("no round")
	}
	a.sweep(start.Add(59 * time.Minute))
	roundDone(t, a, k)
	if got := calls.Load(); got != first {
		t.Fatalf("another round within the hour: %d checks, then %d", first, got)
	}
	a.sweep(start.Add(61 * time.Minute))
	roundDone(t, a, k)
	if got := calls.Load(); got == first {
		t.Fatal("no round after the hour")
	}
}

// Rounds real requests started count against the same budget.
func TestAuditTimerRespectsTheBudget(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	a := recheckAudits(&bad, &calls)
	k := auditKey{peer.ID("srv"), "m"}
	now := time.Now()
	pa := &peerAudit{State: "suspect", lastBad: now.Add(-time.Hour)}
	for i := range auditsPerHr {
		pa.rounds = append(pa.rounds, now.Add(-time.Duration(i+1)*time.Minute))
	}
	a.peers[k] = pa
	a.sweep(now)
	roundDone(t, a, k)
	if calls.Load() != 0 {
		t.Fatalf("%d checks with the budget spent", calls.Load())
	}
	if n := len(pa.rounds); n != auditsPerHr {
		t.Fatalf("%d rounds recorded", n)
	}
}

// A sweep starts only so many rounds at once, and the rest wait their turn.
func TestAuditTimerStartsAFewAtATime(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	a := recheckAudits(&bad, &calls)
	now := time.Now()
	for i := range recheckBatch * 3 {
		a.peers[auditKey{peer.ID(fmt.Sprint("srv", i)), "m"}] = &peerAudit{State: "suspect", lastBad: now}
	}
	a.sweep(now)
	for k := range a.peers {
		roundDone(t, a, k)
	}
	if got := calls.Load(); got != recheckBatch {
		t.Fatalf("%d rounds, want %d", got, recheckBatch)
	}
}

// No bad result for a day makes a suspect unchecked again, and a bad result
// starts the day over.
func TestAuditSuspectDecays(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	a := recheckAudits(&bad, &calls)
	k := auditKey{peer.ID("srv"), "m"}
	now := time.Now()
	a.peers[k] = &peerAudit{State: "suspect", lastBad: now.Add(-suspectDecay - time.Minute)}
	a.sweep(now)
	roundDone(t, a, k)
	if a.peers[k].State != "unchecked" || calls.Load() != 0 {
		t.Fatalf("state %s, %d checks", a.peers[k].State, calls.Load())
	}
	if _, demoted := a.rank(k.p, k.model); demoted {
		t.Fatal("still demoted after the decay")
	}

	// A round that keeps finding bad answers keeps the state.
	bad.Store(true)
	a.peers[k] = &peerAudit{State: "suspect", lastBad: now.Add(-suspectDecay + time.Hour)}
	a.sweep(now)
	roundDone(t, a, k)
	if since := time.Since(a.peers[k].lastBad); since > time.Minute {
		t.Fatalf("the bad result did not count: last bad %v ago", since)
	}
	a.sweep(now.Add(2 * time.Hour))
	roundDone(t, a, k)
	if a.peers[k].State == "unchecked" {
		t.Fatal("decayed in spite of a recent bad result")
	}
}

// A live failure does not decay: it has its own time limit.
func TestAuditFailureIsNotASuspect(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	a := recheckAudits(&bad, &calls)
	k := auditKey{peer.ID("srv"), "m"}
	now := time.Now()
	a.peers[k] = &peerAudit{State: "fail", Until: now.Add(time.Hour), lastBad: now.Add(-2 * suspectDecay)}
	a.sweep(now)
	roundDone(t, a, k)
	if a.peers[k].State != "fail" || calls.Load() != 0 {
		t.Fatalf("state %s, %d checks", a.peers[k].State, calls.Load())
	}
}

// At the cap a new peer pushes out the entry used longest ago, except a live
// failure or a running round.
func TestAuditEvictsOldestAtTheCap(t *testing.T) {
	var bad atomic.Bool
	var calls atomic.Int32
	a := recheckAudits(&bad, &calls)
	now := time.Now()
	key := func(i int) auditKey { return auditKey{peer.ID(fmt.Sprint("p", i)), "m"} }
	for i := range maxAudited {
		a.peers[key(i)] = &peerAudit{State: "pass", seen: now.Add(time.Duration(i) * time.Second)}
	}
	// The oldest two cannot go; the third can.
	a.peers[key(0)].State, a.peers[key(0)].Until = "fail", now.Add(time.Hour)
	a.peers[key(1)].running = true

	newcomer := auditKey{peer.ID("newcomer"), "m"}
	a.observe(newcomer.p, newcomer.model, []byte(`{}`))
	roundDone(t, a, newcomer)
	if _, ok := a.peers[newcomer]; !ok {
		t.Fatal("the new peer was ignored at the cap")
	}
	if len(a.peers) != maxAudited {
		t.Fatalf("%d entries", len(a.peers))
	}
	if _, ok := a.peers[key(2)]; ok {
		t.Fatal("the oldest evictable entry stayed")
	}
	for _, i := range []int{0, 1, 3} {
		if _, ok := a.peers[key(i)]; !ok {
			t.Fatalf("entry %d went", i)
		}
	}
	// An expired failure counts as any other entry.
	a.peers[key(0)].Until = now.Add(-time.Second)
	a.observe(peer.ID("second"), "m", []byte(`{}`))
	roundDone(t, a, auditKey{peer.ID("second"), "m"})
	if _, ok := a.peers[key(0)]; ok {
		t.Fatal("an expired failure stayed")
	}

	// With only live failures left the new peer is not remembered.
	for k := range a.peers {
		delete(a.peers, k)
	}
	for i := range maxAudited {
		a.peers[key(i)] = &peerAudit{State: "fail", Until: now.Add(time.Hour)}
	}
	a.observe(peer.ID("third"), "m", []byte(`{}`))
	if _, ok := a.peers[auditKey{peer.ID("third"), "m"}]; ok || len(a.peers) != maxAudited {
		t.Fatalf("%d entries, a failure was evicted", len(a.peers))
	}
}

// A node started with --audit runs the timer.
func TestAuditTimerRuns(t *testing.T) {
	defer func(d time.Duration) { recheckEvery = d }(recheckEvery)
	recheckEvery = 10 * time.Millisecond
	n := testNode(t, t.Context(), Config{Audit: true, AuditEvery: 1})
	a := n.audits
	var calls atomic.Int32
	k := auditKey{peer.ID("srv"), "m"}
	a.mu.Lock()
	a.checkFn = func(peer.ID, string, reqParams, map[peer.ID]bool) (auditCheck, bool) {
		calls.Add(1)
		return auditCheck{Stats: &auditStats{}, Auditor: "a"}, true
	}
	a.peers[k] = &peerAudit{State: "suspect", lastBad: time.Now()}
	a.mu.Unlock()
	eventually(t, 5*time.Second, "the timer never checked the suspect", func() bool { return calls.Load() > 0 })
}
