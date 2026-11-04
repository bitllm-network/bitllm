package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// takeSlot fills one serving slot on n and returns its ticket.
func takeSlot(t *testing.T, n *Node, p peer.ID, now time.Time) ticket {
	t.Helper()
	s, msg, a := n.admit(p, "", now, 0, 0)
	if s != 0 || a.w != nil {
		t.Fatalf("admit: %d %s", s, msg)
	}
	return a.t
}

// queueDirect puts a waiter of an explicit class in the queue, for the cases
// the access rules cannot set up alone.
func queueDirect(t *testing.T, n *Node, p peer.ID, class int, at time.Time) *waiter {
	t.Helper()
	w := &waiter{peer: p, model: "m", class: class, at: at, open: class == classOpen,
		ready: make(chan struct{})}
	w.t.peer = p
	n.qmu.Lock()
	ok := n.enqueueLocked(w)
	n.qmu.Unlock()
	if !ok {
		t.Fatal("queue full")
	}
	return w
}

// TestQueueFIFO: with one slot, requests are served in the order they asked,
// and each tells the client where it stands.
func TestQueueFIFO(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 1})
	a, b, c := randomPeer(t), randomPeer(t), randomPeer(t)
	now := time.Now()

	first := takeSlot(t, n, a, now)
	s, _, wb := n.admit(b, "m", now, time.Minute, 0)
	if s != 202 || wb.w == nil {
		t.Fatalf("second request: %d %+v", s, wb)
	}
	s, _, wc := n.admit(c, "m", now.Add(time.Millisecond), time.Minute, 0)
	if s != 202 || wc.w == nil {
		t.Fatalf("third request: %d %+v", s, wc)
	}
	if p := n.position(wb.w); p != 1 {
		t.Fatalf("first waiter position = %d, want 1", p)
	}
	if p := n.position(wc.w); p != 2 {
		t.Fatalf("second waiter position = %d, want 2", p)
	}
	if d := n.queueDepth(); d != 2 {
		t.Fatalf("queue depth = %d, want 2", d)
	}

	n.done(first, 0, now.Add(100*time.Millisecond))
	if eta := n.eta(2); eta != 100 {
		t.Fatalf("eta(2) = %d ms, want 100 from the one job just served", eta)
	}
	select {
	case <-wb.w.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("first waiter was not granted the free slot")
	}
	select {
	case <-wc.w.ready:
		t.Fatal("second waiter was granted before the first job ended")
	default:
	}
	n.done(wb.w.t, 0, now)
	select {
	case <-wc.w.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("second waiter was not granted")
	}
	n.done(wc.w.t, 0, now)
	if d := n.queueDepth(); d != 0 {
		t.Fatalf("queue depth = %d after all served, want 0", d)
	}
	if n.inflight.Load() != 0 {
		t.Fatalf("inflight = %d, want 0", n.inflight.Load())
	}
}

// TestQueueClasses: a contributor outranks a member, a member outranks an open
// server's uninvited caller, and an idle-only caller waits for the last slot.
func TestQueueClasses(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 1, Open: true})
	a, b, c, d := randomPeer(t), randomPeer(t), randomPeer(t), randomPeer(t)
	now := time.Now()
	n.inflight.Add(1) // a job of the owner's own holds the only slot

	idle := queueDirect(t, n, a, classIdle, now)
	open := queueDirect(t, n, b, classOpen, now.Add(time.Millisecond))
	member := queueDirect(t, n, c, classMember, now.Add(2*time.Millisecond))
	contrib := queueDirect(t, n, d, classContributor, now.Add(3*time.Millisecond))

	n.inflight.Add(-1)
	n.dispatch()
	select {
	case <-contrib.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("contributor did not go first")
	}
	for _, w := range []*waiter{idle, open, member} {
		select {
		case <-w.ready:
			t.Fatal("a lower class was granted while the contributor held the only slot")
		default:
		}
	}
	n.done(contrib.t, 0, now)
	select {
	case <-member.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("member did not follow the contributor")
	}
	n.done(member.t, 0, now)
	select {
	case <-open.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("open caller did not follow the member")
	}
	n.done(open.t, 0, now)
	select {
	case <-idle.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("idle-only caller did not get the idle server")
	}
	n.done(idle.t, 0, now)
	if n.inflight.Load() != 0 || n.openInflight.Load() != 0 {
		t.Fatalf("slots leaked: %d %d", n.inflight.Load(), n.openInflight.Load())
	}
}

// TestQueueAging: a request that has waited moves up a class, so a steady
// trickle of members cannot keep the open pool waiting forever.
func TestQueueAging(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 1, Open: true})
	member, open := randomPeer(t), randomPeer(t)
	now := time.Now()
	slot := takeSlot(t, n, randomPeer(t), now)

	// The open caller has waited two steps, so it ranks as a member, and it
	// is older than the member who just arrived.
	wm := queueDirect(t, n, member, classMember, now.Add(-time.Second))
	wo := queueDirect(t, n, open, classOpen, now.Add(-2*ageStep))
	if p := n.position(wo); p != 1 {
		t.Fatalf("aged waiter position = %d, want 1", p)
	}
	n.done(slot, 0, now)
	select {
	case <-wo.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("an aged waiter did not move ahead of a fresh higher class")
	}
	select {
	case <-wm.ready:
		t.Fatal("the fresh member was served before the aged waiter")
	default:
	}
	n.done(wo.t, 0, now)
	n.done(wm.t, 0, now)
}

// TestQueueFairness: one peer may not hold every slot while another waits.
func TestQueueFairness(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 4})
	a, b := randomPeer(t), randomPeer(t)
	now := time.Now()

	t1 := takeSlot(t, n, a, now)
	t2 := takeSlot(t, n, a, now)
	if n.perPeerSlots() != 2 {
		t.Fatalf("perPeerSlots = %d, want 2", n.perPeerSlots())
	}
	b, c := randomPeer(t), randomPeer(t)
	wa := queueDirect(t, n, a, classMember, now)
	wb := queueDirect(t, n, b, classMember, now.Add(time.Millisecond))
	wc := queueDirect(t, n, c, classMember, now.Add(2*time.Millisecond))

	// Two slots are free, but a already holds its share while b and c wait.
	n.dispatch()
	for _, w := range []*waiter{wb, wc} {
		select {
		case <-w.ready:
		case <-time.After(2 * time.Second):
			t.Fatal("a peer with no slots was not served while another held its share")
		}
	}
	select {
	case <-wa.ready:
		t.Fatal("one peer took a third slot while others had none")
	default:
	}

	// With b and c served and nobody else waiting, a gets its share back.
	n.done(t1, 0, now)
	n.done(t2, 0, now)
	n.dispatch()
	select {
	case <-wa.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("a did not get a slot once nobody else was waiting")
	}
	n.done(wb.t, 0, now)
	n.done(wc.t, 0, now)
	n.done(wa.t, 0, now)
}

// TestQueueFull: the queue is bounded, and a full one refuses at once.
func TestQueueFull(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 1})
	now := time.Now()
	slot := takeSlot(t, n, randomPeer(t), now)
	for i := 0; i < maxQueue; i++ {
		if s, msg, a := n.admit(randomPeer(t), "m", now, time.Minute, 0); s != 202 || a.w == nil {
			t.Fatalf("waiter %d: %d %s", i, s, msg)
		}
	}
	if s, msg, _ := n.admit(randomPeer(t), "m", now, time.Minute, 0); s != 429 || msg != msgQueueFull {
		t.Fatalf("queue over the cap: %d %s", s, msg)
	}
	if d := n.queueDepth(); d != maxQueue {
		t.Fatalf("queue depth = %d, want %d", d, maxQueue)
	}
	if got := n.refused.queue.Load(); got != 1 {
		t.Fatalf("refusals.queue = %d, want 1", got)
	}
	n.done(slot, 0, now)
}

// TestQueueCancel: a caller that gives up loses its place, and one that was
// already granted keeps the slot it was given.
func TestQueueCancel(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 1})
	now := time.Now()
	slot := takeSlot(t, n, randomPeer(t), now)

	_, _, a := n.admit(randomPeer(t), "m", now, time.Minute, 0)
	if !n.cancelWait(a.w) {
		t.Fatal("cancel of a waiting request reported it was already granted")
	}
	if d := n.queueDepth(); d != 0 {
		t.Fatalf("queue depth = %d after cancel, want 0", d)
	}
	n.done(slot, 0, now)

	granted := takeSlot(t, n, randomPeer(t), now)
	_, _, b := n.admit(randomPeer(t), "m", now, time.Minute, 0)
	n.done(granted, 0, now) // grants b
	select {
	case <-b.w.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not granted")
	}
	if n.cancelWait(b.w) {
		t.Fatal("cancel of a granted request reported it was still waiting")
	}
	n.done(b.w.t, 0, now)
	if n.inflight.Load() != 0 {
		t.Fatalf("inflight = %d, want 0", n.inflight.Load())
	}
}

// TestQuotaReservation: an admitted job reserves its worst case, so two
// parallel jobs cannot both pass the same remaining quota.
func TestQuotaReservation(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 4, FreeDaily: 100})
	p := randomPeer(t)
	now := time.Now()
	key := "node:" + p.String()

	s, msg, first := n.admit(p, "", now, 0, 60)
	if s != 0 || first.t.reserved != 60 {
		t.Fatalf("first admit: %d %s reserved=%d", s, msg, first.t.reserved)
	}
	s, msg, second := n.admit(p, "", now, 0, 60)
	if s != 0 || second.t.reserved != 40 {
		t.Fatalf("second admit: %d %s reserved=%d, want the 40 credits left", s, msg, second.t.reserved)
	}
	if s, msg, _ := n.admit(p, "", now, 0, 10); s != 429 || msg != msgQuotaSpent {
		t.Fatalf("admit with no room: %d %s", s, msg)
	}

	n.done(first.t, 10, now)
	n.quota.mu.Lock()
	used := n.quota.Used[key]
	n.quota.mu.Unlock()
	if used != 10 {
		t.Fatalf("used = %d, want 10", used)
	}
	if room := n.quota.room(key, 100, now); room != 50 {
		t.Fatalf("room = %d, want 50 (100 - 10 used - 40 reserved)", room)
	}
	n.done(second.t, 20, now)
	if room := n.quota.room(key, 100, now); room != 70 {
		t.Fatalf("room = %d after both jobs, want 70", room)
	}

	// A request waiting in the queue holds its reservation too, and gives it
	// back when it gives up.
	q := testNode(t, ctx, Config{Concurrency: 1, FreeDaily: 100})
	p2 := randomPeer(t)
	key2 := "node:" + p2.String()
	slot := takeSlot(t, q, p2, now)
	if s, msg, w := q.admit(p2, "", now, time.Minute, 60); s != 202 || w.w == nil {
		t.Fatalf("queued admit: %d %s", s, msg)
	} else if room := q.quota.room(key2, 100, now); room != 40 {
		t.Fatalf("room while one request waits = %d, want 40", room)
	} else if !q.cancelWait(w.w) {
		t.Fatal("queued request was already granted")
	}
	if room := q.quota.room(key2, 100, now); room != 100 {
		t.Fatalf("room after the waiter gave up = %d, want 100", room)
	}
	q.done(slot, 0, now)
}

// TestQueueView: GET /queue says what runs and what waits.
func TestQueueView(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Concurrency: 1})
	now := time.Now()
	takeSlot(t, n, randomPeer(t), now)
	n.admit(randomPeer(t), "m", now, time.Minute, 0)

	w := gwReq("GET", "/queue", nil)
	rec := httptest.NewRecorder()
	n.Gateway().ServeHTTP(rec, w)
	if rec.Code != 200 {
		t.Fatalf("GET /queue: %d %s", rec.Code, rec.Body)
	}
	var v struct {
		Slots    int            `json:"slots"`
		Inflight int32          `json:"inflight"`
		Queued   int            `json:"queued"`
		PerModel map[string]int `json:"per_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Slots != 1 || v.Inflight != 1 || v.Queued != 1 || v.PerModel["m"] != 1 {
		t.Fatalf("GET /queue = %+v", v)
	}
}
