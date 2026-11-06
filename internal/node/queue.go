package node

// A bounded queue of peer requests for serving slots.
//
// Before this, a peer that found every slot taken was told "busy" and had to
// ask again; the gateway did that once a second for a minute, opening a new
// stream every time. Here the request waits on one stream instead, in a queue
// ordered by class and then by arrival. A waiter gains one class every
// ageStep, but never past the best one, so the classes keep their meaning for
// a short wait and then arrival decides: a request that has waited long
// enough is not pushed back by every later request of a lower class, and
// among equals the longest wait goes first.
//
// Slots are the server's --concurrency. A waiter costs no slot until it is
// granted one, so a long queue does not consume GPU time. The queue is bounded
// (maxQueue waiters) and each peer may hold only part of the slots, so one
// client cannot fill the server and starve the rest.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// maxQueue is the most waiters one server holds. Peer ids are free, so
	// without a cap anyone could make the queue grow without limit.
	maxQueue = protocol.MaxQueueDepth

	// maxWait is the longest a peer may ask to wait, whatever it sends.
	maxWait = 5 * time.Minute

	// ageStep is how long a waiter must wait to move up one class. Without
	// it, a steady trickle of members would starve the open pool, since a
	// class always beats an older request of a lower class.
	ageStep = 20 * time.Second

	// maxPerPeerQueue is the most places one peer may hold at once. Peer
	// ids are free, so without it one id (or a few) could fill the queue
	// and shut members out.
	maxPerPeerQueue = 8
)

// Request classes, best first. They mirror the three access rules: a
// contributor (a node that served lately) outranks a member, a member
// outranks an uninvited node on an open server, and an uninvited node on a
// closed server is served only when nothing else runs.
const (
	classContributor = iota
	classMember
	classOpen
	classIdle
)

// ageClass is a waiter's class after it has waited: one step better per
// ageStep, but no better than a contributor.
func ageClass(w *waiter, now time.Time) int {
	bump := 0
	if age := now.Sub(w.at); age > 0 {
		bump = int(age / ageStep)
	}
	return max(classContributor, w.class-bump)
}

// waiter is one queued request.
type waiter struct {
	peer   peer.ID
	model  string
	class  int
	at     time.Time
	key    string        // quota key, "" when the job is not charged
	limit  int64         // daily limit for key, 0 = none
	amount int64         // credits to reserve for the job while it runs
	open   bool          // an uninvited node served on an open server
	pool   int64         // daily limit of the shared open pool, 0 = none
	acct   string        // the peer's linked account, "" when it has none
	ready  chan struct{} // closed when granted a slot
	t      ticket

	// evicted is closed when a request of a better class took this place in a
	// full queue. Eviction and a grant both happen under Node.qmu and only on a
	// pending waiter, so exactly one of ready and evicted ever closes.
	evicted chan struct{}

	// state is guarded by Node.qmu: pending, granted or canceled.
	state int
}

const (
	waitPending = iota
	waitGranted
	waitCanceled
)

// queueDepth is how many requests wait for a slot right now, for the
// announce and GET /queue.
func (n *Node) queueDepth() int {
	n.qmu.Lock()
	defer n.qmu.Unlock()
	depth := 0
	for _, w := range n.waiters {
		if w.state == waitPending {
			depth++
		}
	}
	return depth
}

// perPeerSlots is how many slots one peer may hold at once. With a single
// slot there is nothing to divide; above that, half the server, and never
// less than one.
func (n *Node) perPeerSlots() int32 {
	if n.cfg.Concurrency <= 1 {
		return 1
	}
	return int32(max(1, n.cfg.Concurrency/2))
}

// tryTakeLocked takes one slot for a request of class from p, if one fits.
// The caller holds q.mu.
func (n *Node) tryTakeLocked(p peer.ID, class int) bool {
	if n.inflight.Load() >= int32(n.cfg.Concurrency) {
		return false
	}
	// An uninvited node on a closed server is served only when idle: no
	// other job, admitted or queued ahead of it, may be running.
	if class == classIdle && n.inflight.Load() > 0 {
		return false
	}
	if class == classOpen && n.openInflight.Load() >= n.openSlots() {
		return false
	}
	if n.peerSlots[p] >= n.perPeerSlots() && n.otherWaiterLocked(p) {
		return false
	}
	n.inflight.Add(1)
	if class == classOpen {
		n.openInflight.Add(1)
	}
	n.peerSlots[p]++
	return true
}

// releaseLocked gives back the slot a granted waiter or a served job held.
func (n *Node) releaseLocked(t ticket) {
	n.inflight.Add(-1)
	if t.open {
		n.openInflight.Add(-1)
	}
	if n.peerSlots[t.peer] > 0 {
		n.peerSlots[t.peer]--
	}
	if n.peerSlots[t.peer] == 0 {
		delete(n.peerSlots, t.peer)
	}
}

// queueCountsLocked is how many requests wait, and how many of them belong to
// w's peer or to the account that peer is linked to.
func (n *Node) queueCountsLocked(w *waiter) (pending, mine int) {
	for _, other := range n.waiters {
		if other.state == waitPending {
			pending++
			if other.peer == w.peer || (w.acct != "" && other.acct == w.acct) {
				mine++
			}
		}
	}
	return pending, mine
}

// enqueue adds a waiter if there is room. The caller holds q.mu.
func (n *Node) enqueueLocked(w *waiter) bool {
	pending, mine := n.queueCountsLocked(w)
	if pending >= maxQueue || mine >= maxPerPeerQueue {
		return false
	}
	if w.evicted == nil {
		w.evicted = make(chan struct{})
	}
	n.waiters = append(n.waiters, w)
	return true
}

// evictForLocked makes room for w in a full queue: the youngest waiter of the
// worst class that is strictly worse than w loses its place. It compares the
// classes the access rules gave, not the aged ones, or a queue that has waited
// long enough could no longer make room for anyone. The caller must return the
// waiter's reservations (dropReservation) after unlocking.
func (n *Node) evictForLocked(w *waiter) *waiter {
	pending, mine := n.queueCountsLocked(w)
	if pending < maxQueue || mine >= maxPerPeerQueue {
		return nil
	}
	var victim *waiter
	for _, other := range n.waiters {
		if other.state != waitPending || other.class <= w.class {
			continue
		}
		if victim == nil || other.class > victim.class ||
			(other.class == victim.class && !other.at.Before(victim.at)) {
			victim = other
		}
	}
	if victim != nil {
		victim.state = waitCanceled
		n.dropLocked(victim)
		close(victim.evicted)
	}
	return victim
}

// dropReservation gives back the credits a waiter that lost its place held.
func (n *Node) dropReservation(w *waiter, now time.Time) {
	n.quota.Drop(w.t.key, w.t.reserved, now)
	if w.open {
		n.quota.Drop(openPool, w.t.poolReserved, now)
	}
}

// localTake holds a slot for a request from this machine's own gateway. It is
// never refused, even past --concurrency, and has no per-peer accounting. What
// it changes is what peers see: the announced free count is honest, and they
// wait for the slot.
func (n *Node) localTake() {
	n.qmu.Lock()
	n.inflight.Add(1)
	n.qmu.Unlock()
}

// localDone gives the slot back and grants it to whoever waits.
func (n *Node) localDone() {
	n.qmu.Lock()
	n.inflight.Add(-1)
	n.dispatchLocked()
	n.qmu.Unlock()
}

// dispatch grants free slots to the waiters that fit, best class first and
// oldest first within a class. The caller holds q.mu.
func (n *Node) dispatchLocked() {
	for {
		w := n.pickLocked()
		if w == nil {
			return
		}
		w.state = waitGranted
		// A canceled waiter is skipped in pickLocked, so it cannot be here.
		close(w.ready)
	}
}

// pickLocked takes the best waiter that fits and withholds a slot for it.
// A waiter that does not fit (its peer holds enough slots, or it is an
// idle-only waiter while a job runs) is skipped, so another can go first.
func (n *Node) pickLocked() *waiter {
	now := time.Now()
	var best *waiter
	for _, w := range n.waiters {
		if w.state != waitPending || !n.fitsLocked(w) {
			continue
		}
		if best == nil || ageClass(w, now) < ageClass(best, now) ||
			(ageClass(w, now) == ageClass(best, now) && w.at.Before(best.at)) {
			best = w
		}
	}
	if best == nil {
		return nil
	}
	n.tryTakeLocked(best.peer, best.class) // it fits, so this cannot fail
	best.t.peer, best.t.open = best.peer, best.open
	best.t.start = now
	n.dropLocked(best)
	return best
}

// fitsLocked reports whether w could take a slot right now, without taking it.
func (n *Node) fitsLocked(w *waiter) bool {
	if n.inflight.Load() >= int32(n.cfg.Concurrency) {
		return false
	}
	if w.class == classIdle && n.inflight.Load() > 0 {
		return false
	}
	if w.class == classOpen && n.openInflight.Load() >= n.openSlots() {
		return false
	}
	return n.peerSlots[w.peer] < n.perPeerSlots() || !n.otherWaiterLocked(w.peer)
}

// otherWaiterLocked reports whether a peer other than p has a request
// waiting, which is when one peer must give up its spare slots to it.
func (n *Node) otherWaiterLocked(p peer.ID) bool {
	for _, w := range n.waiters {
		if w.state == waitPending && w.peer != p {
			return true
		}
	}
	return false
}

func (n *Node) dropLocked(w *waiter) {
	for i, other := range n.waiters {
		if other == w {
			n.waiters = append(n.waiters[:i], n.waiters[i+1:]...)
			return
		}
	}
}

// cancelWait gives up a place in the queue. It reports false when the waiter
// was already granted a slot, which the caller must then use.
func (n *Node) cancelWait(w *waiter) bool {
	n.qmu.Lock()
	if w.state != waitPending {
		state := w.state
		n.qmu.Unlock()
		return state == waitCanceled
	}
	w.state = waitCanceled
	n.dropLocked(w)
	// Losing a place can free a slot another peer was waiting for (its
	// share of the slots, say), so look again before unlocking.
	n.dispatchLocked()
	n.qmu.Unlock()
	// The place is gone, so the credits it held go back to the day's room.
	n.dropReservation(w, time.Now())
	return true
}

// queueView is GET /queue: what is running and what is waiting, per model.
func (n *Node) queueView(w http.ResponseWriter, r *http.Request) {
	n.qmu.Lock()
	models := map[string]int{}
	oldest := time.Duration(0)
	for _, wait := range n.waiters {
		if wait.state != waitPending {
			continue
		}
		models[wait.model]++
		if age := time.Since(wait.at); age > oldest {
			oldest = age
		}
	}
	slots := int32(n.cfg.Concurrency)
	inflight, open := n.inflight.Load(), n.openInflight.Load()
	n.qmu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"slots":      slots,
		"inflight":   inflight,
		"open":       open,
		"open_slots": n.openSlots(),
		"queued":     n.queueDepth(),
		"oldest_ms":  oldest.Milliseconds(),
		"per_model":  models,
		"refusals":   n.refused.snapshot(),
	})
}

// eta estimates in milliseconds how long a waiter at position pos still
// waits, from the average time recent jobs took and how many run at once.
// Zero means unknown (no job served yet).
func (n *Node) eta(pos int) int64 {
	avg := n.svc.Load()
	if avg <= 0 || pos <= 1 {
		return 0
	}
	slots := max(n.cfg.Concurrency, 1)
	return int64(time.Duration(pos-1) * time.Duration(avg) / time.Duration(slots) / time.Millisecond)
}

// position is where w stands in the queue: 1 is next, under the same order
// dispatch uses.
func (n *Node) position(w *waiter) int {
	n.qmu.Lock()
	defer n.qmu.Unlock()
	now := time.Now()
	pos := 1
	for _, other := range n.waiters {
		if other == w || other.state != waitPending {
			continue
		}
		if ageClass(other, now) < ageClass(w, now) ||
			(ageClass(other, now) == ageClass(w, now) && other.at.Before(w.at)) {
			pos++
		}
	}
	return pos
}

// dispatch grants whatever the queue can take now.
func (n *Node) dispatch() {
	n.qmu.Lock()
	defer n.qmu.Unlock()
	n.dispatchLocked()
}
