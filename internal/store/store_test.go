package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteFileReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "two" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
	fi, _ := os.Stat(path)
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // Windows has no such bits
		t.Fatalf("mode %v", fi.Mode())
	}
}

// A write that fails partway leaves the old file whole.
func TestWriteStreamFailureKeepsOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := WriteStream(path, 0o600, func(w io.Writer) error {
		w.Write([]byte("partial"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

func TestWriteFileMissingDirectory(t *testing.T) {
	if err := WriteFile(filepath.Join(t.TempDir(), "no", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("no error for a missing directory")
	}
}

func counter(t *testing.T, delay time.Duration) (*Debounced, string, *atomic.Int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	var value atomic.Int64
	d := NewDebounced(path, 0o600, delay, func() ([]byte, error) {
		return []byte{byte(value.Load())}, nil
	})
	return d, path, &value
}

func TestMarksCoalesceIntoOneWrite(t *testing.T) {
	d, path, value := counter(t, 50*time.Millisecond)
	for i := range 100 {
		value.Store(int64(i))
		d.Mark()
	}
	if d.Writes() != 0 {
		t.Fatal("wrote before the delay")
	}
	deadline := time.Now().Add(5 * time.Second)
	for d.Writes() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := d.Writes(); got != 1 {
		t.Fatalf("%d writes, want 1", got)
	}
	if b, _ := os.ReadFile(path); len(b) != 1 || b[0] != 99 {
		t.Fatalf("content %v", b)
	}
}

func TestSteadyMarksStillWrite(t *testing.T) {
	d, _, _ := counter(t, 30*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for d.Writes() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("marks kept pushing the write back")
		}
		d.Mark()
		time.Sleep(time.Millisecond)
	}
}

func TestFlushWritesNowAndOnlyWhenChanged(t *testing.T) {
	d, path, value := counter(t, time.Hour)
	if err := d.Flush(); err != nil || d.Writes() != 0 {
		t.Fatalf("flush with nothing marked: %v, %d writes", err, d.Writes())
	}
	value.Store(7)
	d.Mark()
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); len(b) != 1 || b[0] != 7 {
		t.Fatalf("content %v", b)
	}
	d.Flush()
	if d.Writes() != 1 {
		t.Fatalf("%d writes, want 1", d.Writes())
	}
}

func TestFailedWriteIsRetried(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "later")
	d := NewDebounced(filepath.Join(dir, "f"), 0o600, time.Hour, func() ([]byte, error) { return []byte("x"), nil })
	d.Mark()
	if err := d.Flush(); err == nil {
		t.Fatal("no error for a missing directory")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.Flush(); err != nil || d.Writes() != 1 {
		t.Fatalf("retry: %v, %d writes", err, d.Writes())
	}
}

func TestSnapshotErrorStaysMarked(t *testing.T) {
	boom := errors.New("boom")
	var fail atomic.Bool
	fail.Store(true)
	d := NewDebounced(filepath.Join(t.TempDir(), "f"), 0o600, time.Hour, func() ([]byte, error) {
		if fail.Load() {
			return nil, boom
		}
		return []byte("x"), nil
	})
	d.Mark()
	if err := d.Flush(); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	fail.Store(false)
	if err := d.Flush(); err != nil || d.Writes() != 1 {
		t.Fatalf("retry: %v, %d writes", err, d.Writes())
	}
}

func TestCloseFlushesAndStopsTheClock(t *testing.T) {
	d, _, _ := counter(t, 10*time.Millisecond)
	d.Mark()
	if err := d.Close(); err != nil || d.Writes() != 1 {
		t.Fatalf("close: %v, %d writes", err, d.Writes())
	}
	d.Mark()
	time.Sleep(60 * time.Millisecond)
	if d.Writes() != 1 {
		t.Fatal("a closed writer scheduled a write")
	}
	if err := d.Flush(); err != nil || d.Writes() != 2 {
		t.Fatalf("flush after close: %v, %d writes", err, d.Writes())
	}
}

// The owner's lock is held across Mark and taken by snapshot.
func TestMarkUnderOwnerLockDoesNotDeadlock(t *testing.T) {
	var owner sync.Mutex
	d := NewDebounced(filepath.Join(t.TempDir(), "f"), 0o600, time.Millisecond, func() ([]byte, error) {
		owner.Lock()
		defer owner.Unlock()
		return []byte("x"), nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2000 {
			owner.Lock()
			d.Mark()
			owner.Unlock()
		}
		d.Flush()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock")
	}
}

func TestForEachVisitsEveryIndexOnce(t *testing.T) {
	for _, n := range []int{0, 1, 1000} {
		seen := make([]atomic.Int32, n)
		ForEach(n, func(i int) { seen[i].Add(1) })
		for i := range seen {
			if seen[i].Load() != 1 {
				t.Fatalf("n=%d: index %d ran %d times", n, i, seen[i].Load())
			}
		}
	}
}
