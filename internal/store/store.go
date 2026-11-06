// Package store writes a node's state files. A write replaces the whole file
// atomically and reaches the disk before it returns, and a Debounced writer
// lets a busy store turn many changes into one write.
package store

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultDelay is how long a Debounced writer collects changes before writing.
const DefaultDelay = 2 * time.Second

// WriteFile replaces path with data: a temporary file beside it, fsync, rename,
// fsync of the directory. A reader sees the old file or the new one, never part
// of either.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return WriteStream(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteStream is WriteFile for content too large to hold in memory.
func WriteStream(path string, perm os.FileMode, fill func(io.Writer) error) (err error) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err = fill(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes the rename durable. It is best effort: not every platform can
// sync a directory, and the file itself is already safe.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Debounced writes one file on behalf of a store. Mark says the content
// changed; the write follows within Delay, however many marks come in between.
//
// Snapshot takes the owner's lock, so Mark may be called with that lock held
// and Flush and Close may not.
type Debounced struct {
	path     string
	perm     os.FileMode
	delay    time.Duration
	snapshot func() ([]byte, error)

	writing sync.Mutex // one write at a time, held across snapshot

	mu     sync.Mutex // guards timer, dirty and closed; never held across a write
	timer  *time.Timer
	dirty  bool
	closed bool

	writes atomic.Int64
}

// NewDebounced returns a writer for path. A delay of 0 means DefaultDelay.
func NewDebounced(path string, perm os.FileMode, delay time.Duration, snapshot func() ([]byte, error)) *Debounced {
	if delay <= 0 {
		delay = DefaultDelay
	}
	return &Debounced{path: path, perm: perm, delay: delay, snapshot: snapshot}
}

// Mark schedules a write. The first mark starts the clock and later ones do
// not extend it, so a steady stream of changes still reaches the disk.
func (d *Debounced) Mark() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dirty = true
	if d.timer == nil && !d.closed {
		d.timer = time.AfterFunc(d.delay, func() {
			if err := d.Flush(); err != nil {
				log.Printf("save %s: %v", d.path, err)
			}
		})
	}
}

// Flush writes now if anything changed. After a failed write the change stays
// marked, so the next Mark or Flush tries again.
func (d *Debounced) Flush() error {
	d.writing.Lock()
	defer d.writing.Unlock()
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	dirty := d.dirty
	d.dirty = false
	d.mu.Unlock()
	if !dirty {
		return nil
	}
	b, err := d.snapshot()
	if err == nil {
		err = WriteFile(d.path, b, d.perm)
	}
	if err != nil {
		d.mu.Lock()
		d.dirty = true
		d.mu.Unlock()
		return err
	}
	d.writes.Add(1)
	return nil
}

// Close flushes and stops scheduling writes. A later Flush still writes.
func (d *Debounced) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return d.Flush()
}

// Writes counts the file writes so far.
func (d *Debounced) Writes() int64 { return d.writes.Load() }

// ForEach runs fn(0) to fn(n-1) on all cores and returns when they are done.
// Loading a store checks a signature per record, and this is what keeps a large
// one from holding up startup on a machine that has the cores.
func ForEach(n int, fn func(i int)) {
	workers := min(runtime.GOMAXPROCS(0), n)
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}
