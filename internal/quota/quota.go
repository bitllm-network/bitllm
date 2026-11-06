// Package quota counts credits served per account (or per peer, when a node
// has no account) during the current UTC day, on one server. Reservations
// keep parallel jobs from both passing the same remaining quota.
package quota

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/bitllm-org/bitllm/internal/store"
)

// Store is the per-day record. reserved is the part of a job that has been
// admitted but not yet served: a job reserves its worst case when it takes a
// slot and settles the real cost when it ends.
type Store struct {
	mu       sync.Mutex
	save     *store.Debounced
	day      int64
	used     map[string]int64
	reserved map[string]int64
}

// fileQuota is the on-disk shape; reservations are not persisted.
type fileQuota struct {
	Day  int64            `json:"day"`
	Used map[string]int64 `json:"used"`
}

func utcDay(t time.Time) int64 { return t.Unix() / 86400 }

// Open loads the store for the current day, dropping an older day's counts. A
// file that does not parse is logged and treated as empty.
func Open(path string) *Store {
	q := &Store{day: utcDay(time.Now()), used: map[string]int64{}, reserved: map[string]int64{}}
	q.save = store.NewDebounced(path, 0o600, 0, q.snapshot)
	b, err := os.ReadFile(path)
	if err != nil {
		return q
	}
	var f fileQuota
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("quota file %s: %v (starting empty)", path, err)
		return q
	}
	if f.Day == q.day && f.Used != nil {
		q.used = f.Used
	}
	return q
}

func (q *Store) snapshot() ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return json.Marshal(fileQuota{Day: q.day, Used: q.used})
}

// Flush writes pending changes now.
func (q *Store) Flush() error { return q.save.Flush() }

// Close flushes and stops scheduling writes.
func (q *Store) Close() error { return q.save.Close() }

func (q *Store) roll(now time.Time) {
	if d := utcDay(now); d != q.day {
		q.day, q.used, q.reserved = d, map[string]int64{}, map[string]int64{}
	}
}

// Room is what is left of limit for key, after served and reserved credits.
// A limit of 0 means no limit.
func (q *Store) Room(key string, limit int64, now time.Time) int64 {
	if limit <= 0 {
		return 1 << 62
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(now)
	return limit - q.used[key] - q.reserved[key]
}

// Used reports the credits already served to key today.
func (q *Store) Used(key string, now time.Time) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(now)
	return q.used[key]
}

// Take checks that room is left for key and reserves up to amount against
// limit, all in one critical section, so two parallel jobs cannot both pass
// the same remaining quota. It reports false when nothing is left today.
func (q *Store) Take(key string, amount, limit int64, now time.Time) (int64, bool) {
	if key == "" || limit <= 0 {
		return 0, true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(now)
	room := limit - q.used[key] - q.reserved[key]
	if room <= 0 {
		return 0, false
	}
	take := min(max(amount, 0), room)
	q.reserved[key] += take
	return take, true
}

// Drop gives a reservation back, for a job that never started.
func (q *Store) Drop(key string, reserved int64, now time.Time) {
	if key == "" || reserved <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(now)
	q.reserved[key] = max(q.reserved[key]-reserved, 0)
}

// Add charges credits to key without a reservation, for a job that took no
// slot of its own (a test, or a correction).
func (q *Store) Add(key string, credits int64, now time.Time) {
	q.Settle(key, 0, credits, now)
}

// Settle releases a reservation and charges the credits the job used.
func (q *Store) Settle(key string, reserved, credits int64, now time.Time) {
	if key == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(now)
	q.reserved[key] = max(q.reserved[key]-reserved, 0)
	q.used[key] += credits
	q.save.Mark()
}
