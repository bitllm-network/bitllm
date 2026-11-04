package quota

import (
	"testing"
	"time"
)

func TestReserveSettleAndReload(t *testing.T) {
	path := t.TempDir() + "/quota.json"
	now := time.Now()
	q := Open(path)
	took, ok := q.Take("alice", 60, 100, now)
	if !ok || took != 60 {
		t.Fatalf("take = %d %v, want 60 true", took, ok)
	}
	if took, ok := q.Take("alice", 60, 100, now); !ok || took != 40 {
		t.Fatalf("second take = %d %v, want the 40 left", took, ok)
	}
	if _, ok := q.Take("alice", 10, 100, now); ok {
		t.Fatal("reserved past the limit")
	}
	q.Settle("alice", 60, 10, now)
	if got := q.Used("alice", now); got != 10 {
		t.Fatalf("used = %d, want 10", got)
	}
	if got := q.Room("alice", 100, now); got != 50 {
		t.Fatalf("room = %d, want 50 (100 - 10 used - 40 reserved)", got)
	}

	// Only served credits are on disk; a restart starts with no reservations.
	again := Open(path)
	if got := again.Used("alice", now); got != 10 {
		t.Fatalf("after reload: used = %d, want 10", got)
	}
	if got := again.Room("alice", 100, now); got != 90 {
		t.Fatalf("after reload: room = %d, want 90", got)
	}
}

func TestDayRollover(t *testing.T) {
	q := Open(t.TempDir() + "/quota.json")
	today := time.Now()
	q.Add("alice", 25, today)
	if got := q.Used("alice", today); got != 25 {
		t.Fatalf("used = %d, want 25", got)
	}
	tomorrow := today.Add(24 * time.Hour)
	if got := q.Used("alice", tomorrow); got != 0 {
		t.Fatalf("used tomorrow = %d, want 0", got)
	}
	if got := q.Room("alice", 100, tomorrow); got != 100 {
		t.Fatalf("room tomorrow = %d, want 100", got)
	}
}
