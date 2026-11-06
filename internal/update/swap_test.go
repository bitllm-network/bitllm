package update

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
)

// swapFiles is dst holding "old", src holding "new", and a stale previous
// binary, in one directory.
func swapFiles(t *testing.T) (src, dst string) {
	t.Helper()
	dir := t.TempDir()
	src, dst = filepath.Join(dir, "bitllm-new"), filepath.Join(dir, "bitllm")
	for path, body := range map[string]string{src: "new", dst: "old", PrevPath(dst): "stale"} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return src, dst
}

func wantFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: %v", filepath.Base(path), err)
		return
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), b, want)
	}
}

func wantNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists: %v", filepath.Base(path), err)
	}
}

var errInjected = errors.New("injected failure")

// watchedFS is osFS that fails the named calls and, when mustExist is set,
// checks that file exists before and after every call: any moment here could be
// the one a crash lands on.
func watchedFS(t *testing.T, mustExist string, fail ...string) fsOps {
	t.Helper()
	check := func(op string) {
		t.Helper()
		if mustExist == "" {
			return
		}
		if _, err := os.Stat(mustExist); err != nil {
			t.Errorf("around %s: %s is gone: %v", op, filepath.Base(mustExist), err)
		}
	}
	step := func(op string, f func() error) error {
		t.Helper()
		check(op)
		if slices.Contains(fail, op) {
			return errInjected
		}
		err := f()
		check(op)
		return err
	}
	return fsOps{
		remove: func(name string) error {
			return step("remove", func() error { return os.Remove(name) })
		},
		link: func(oldname, newname string) error {
			return step("link", func() error { return os.Link(oldname, newname) })
		},
		rename: func(oldpath, newpath string) error {
			return step("rename", func() error { return os.Rename(oldpath, newpath) })
		},
		copy: func(src, dst string) error {
			return step("copy", func() error { return copyDurable(src, dst) })
		},
		syncDir: func(string) {},
	}
}

// Whichever step fails or is the last before a crash, the binary is there, and
// a failure leaves it as it was.
func TestSwapAtomicKeepsDstAtEveryStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     []string
		wantErr  bool
		wantDst  string
		wantPrev string
	}{
		{name: "no failure", wantDst: "new", wantPrev: "old"},
		{name: "stale prev cannot be removed", fail: []string{"remove"}, wantErr: true, wantDst: "old", wantPrev: "stale"},
		{name: "hard link refused", fail: []string{"link"}, wantDst: "new", wantPrev: "old"},
		{name: "hard link and copy refused", fail: []string{"link", "copy"}, wantErr: true, wantDst: "old"},
		{name: "rename fails", fail: []string{"rename"}, wantErr: true, wantDst: "old", wantPrev: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := swapFiles(t)
			err := swapAtomic(watchedFS(t, dst, tc.fail...), src, dst)
			if (err != nil) != tc.wantErr {
				t.Fatalf("swapAtomic = %v, want error %v", err, tc.wantErr)
			}
			wantFile(t, dst, tc.wantDst)
			if tc.wantPrev == "" {
				wantNoFile(t, PrevPath(dst))
			} else {
				wantFile(t, PrevPath(dst), tc.wantPrev)
			}
			if tc.wantErr {
				wantFile(t, src, "new")
			} else {
				wantNoFile(t, src)
			}
		})
	}
}

// With links working the previous binary is the same file, not a second copy.
func TestSwapHardLinksPrev(t *testing.T) {
	if !swapIsAtomicHere() {
		t.Skip("Windows swaps with renames")
	}
	src, dst := swapFiles(t)
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := Swap(src, dst); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(PrevPath(dst))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the previous binary is not the file that was replaced")
	}
	wantFile(t, dst, "new")
	wantFile(t, PrevPath(dst), "old")
}

// Filesystems that refuse links get a real copy: executable, complete, and with
// no temp file left over.
func TestSwapFallsBackToCopyWhenLinksAreRefused(t *testing.T) {
	src, dst := swapFiles(t)
	fs := osFS
	fs.link = func(string, string) error {
		return &os.LinkError{Op: "link", Err: syscall.EPERM}
	}
	if err := swapAtomic(fs, src, dst); err != nil {
		t.Fatal(err)
	}
	wantFile(t, dst, "new")
	wantFile(t, PrevPath(dst), "old")
	fi, err := os.Stat(PrevPath(dst))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 { // Windows has no such bits
		t.Errorf("previous binary mode = %v, want 0755", fi.Mode().Perm())
	}
	wantNoFile(t, PrevPath(dst)+".tmp")
}

// A crash between the link and the rename leaves prev and dst as one file. The
// next swap still has to end with the old binary as prev and the new one in
// place, which a rename onto prev would silently not do.
func TestSwapAfterACrashBetweenLinkAndRename(t *testing.T) {
	src, dst := swapFiles(t)
	if err := os.Remove(PrevPath(dst)); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(dst, PrevPath(dst)); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if err := swapAtomic(osFS, src, dst); err != nil {
		t.Fatal(err)
	}
	wantFile(t, dst, "new")
	wantFile(t, PrevPath(dst), "old")
}

// With nothing to replace, an earlier previous binary is all there is to go
// back to, so it stays.
func TestSwapWithoutDst(t *testing.T) {
	src, dst := swapFiles(t)
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := swapAtomic(osFS, src, dst); err != nil {
		t.Fatal(err)
	}
	wantFile(t, dst, "new")
	wantFile(t, PrevPath(dst), "stale")
}

// The sequence Windows uses: two renames, and the first undone when the second
// fails.
func TestSwapRenames(t *testing.T) {
	t.Run("no failure", func(t *testing.T) {
		src, dst := swapFiles(t)
		if err := swapRenames(osFS, src, dst); err != nil {
			t.Fatal(err)
		}
		wantFile(t, dst, "new")
		wantFile(t, PrevPath(dst), "old")
		wantNoFile(t, src)
	})
	t.Run("restores dst when the second rename fails", func(t *testing.T) {
		src, dst := swapFiles(t)
		fs := osFS
		fs.rename = func(oldpath, newpath string) error {
			if oldpath == src {
				return errInjected
			}
			return os.Rename(oldpath, newpath)
		}
		if err := swapRenames(fs, src, dst); !errors.Is(err, errInjected) {
			t.Fatalf("swapRenames = %v", err)
		}
		wantFile(t, dst, "old")
		wantFile(t, src, "new")
	})
}

func TestSwapBackAtomic(t *testing.T) {
	_, dst := swapFiles(t)
	if err := swapBackAtomic(watchedFS(t, dst), dst); err != nil {
		t.Fatal(err)
	}
	wantFile(t, dst, "stale")
	wantFile(t, FailedPath(dst), "old")
	wantNoFile(t, PrevPath(dst))
}

func TestSwapBackAtomicFailureChangesNothing(t *testing.T) {
	_, dst := swapFiles(t)
	if err := swapBackAtomic(watchedFS(t, dst, "rename"), dst); !errors.Is(err, errInjected) {
		t.Fatalf("swapBackAtomic = %v", err)
	}
	wantFile(t, dst, "old")
	wantFile(t, PrevPath(dst), "stale")
}

// Keeping the failed binary is a convenience and never stops the rollback.
func TestSwapBackAtomicWithoutKeepingTheFailedFile(t *testing.T) {
	for _, fail := range []string{"remove", "link"} {
		_, dst := swapFiles(t)
		if err := swapBackAtomic(watchedFS(t, dst, fail), dst); err != nil {
			t.Fatalf("with %s failing: %v", fail, err)
		}
		wantFile(t, dst, "stale")
		wantNoFile(t, PrevPath(dst))
	}
}

func TestSwapBackRenames(t *testing.T) {
	t.Run("no failure", func(t *testing.T) {
		_, dst := swapFiles(t)
		if err := swapBackRenames(osFS, dst); err != nil {
			t.Fatal(err)
		}
		wantFile(t, dst, "stale")
		wantFile(t, FailedPath(dst), "old")
		wantNoFile(t, PrevPath(dst))
	})
	t.Run("restores dst when the second rename fails", func(t *testing.T) {
		_, dst := swapFiles(t)
		fs := osFS
		fs.rename = func(oldpath, newpath string) error {
			if oldpath == PrevPath(dst) {
				return errInjected
			}
			return os.Rename(oldpath, newpath)
		}
		if err := swapBackRenames(fs, dst); !errors.Is(err, errInjected) {
			t.Fatalf("swapBackRenames = %v", err)
		}
		wantFile(t, dst, "old")
		wantFile(t, PrevPath(dst), "stale")
	})
}

func TestFailedPath(t *testing.T) {
	if got := FailedPath("/home/u/.local/bin/bitllm"); got != "/home/u/.local/bin/bitllm.failed" {
		t.Errorf("FailedPath = %q", got)
	}
	if got := FailedPath(`C:\bitllm\bitllm.exe`); got != `C:\bitllm\bitllm.failed.exe` {
		t.Errorf("FailedPath(.exe) = %q", got)
	}
}
