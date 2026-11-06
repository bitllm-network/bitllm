package update

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// fsOps are the filesystem calls a swap makes, so a test can fail any one of
// them and look at what is left.
type fsOps struct {
	remove  func(name string) error
	link    func(oldname, newname string) error
	rename  func(oldpath, newpath string) error
	copy    func(src, dst string) error
	syncDir func(dir string)
}

var osFS = fsOps{
	remove:  os.Remove,
	link:    os.Link,
	rename:  os.Rename,
	copy:    copyDurable,
	syncDir: syncDir,
}

// Swap puts src in place of dst. The binary it replaces is kept next to it
// (see PrevPath), so one rename goes back a version. On success src is gone; on
// failure dst is still the binary it was.
//
// Everywhere but Windows dst exists at every step, so a crash at any point
// leaves a binary to start. On Windows the running .exe can be renamed but not
// overwritten, so there the swap is two renames, and callers expect the new
// file to be used at the next start.
func Swap(src, dst string) error {
	if runtime.GOOS == "windows" {
		return swapRenames(osFS, src, dst)
	}
	return swapAtomic(osFS, src, dst)
}

// swapAtomic gives the old binary a second name, then renames src over dst,
// which replaces it in one step.
func swapAtomic(fs fsOps, src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		prev := PrevPath(dst)
		if err := fs.remove(prev); err != nil && !os.IsNotExist(err) {
			return err
		}
		// Some filesystems have no hard links, and the kernel can refuse one on
		// a file this user does not own.
		if err := fs.link(dst, prev); err != nil {
			if err := fs.copy(dst, prev); err != nil {
				return err
			}
		}
	}
	if err := fs.rename(src, dst); err != nil {
		return err
	}
	fs.syncDir(filepath.Dir(dst))
	return nil
}

// swapRenames moves dst out of the way and src into its place, and puts dst
// back if the second rename fails.
func swapRenames(fs fsOps, src, dst string) error {
	prev := PrevPath(dst)
	if err := fs.remove(prev); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		if err := fs.rename(dst, prev); err != nil {
			return err
		}
	}
	if err := fs.rename(src, dst); err != nil {
		if _, e := os.Stat(prev); e == nil {
			fs.rename(prev, dst)
		}
		return err
	}
	return nil
}

// swapBack undoes Swap: the binary at PrevPath(dst) takes the place of dst, and
// the one it replaces stays as FailedPath(dst), so a person can run it to see
// why it failed. Failure leaves dst as it was.
func swapBack(fs fsOps, dst string) error {
	if runtime.GOOS == "windows" {
		return swapBackRenames(fs, dst)
	}
	return swapBackAtomic(fs, dst)
}

func swapBackAtomic(fs fsOps, dst string) error {
	failed := FailedPath(dst)
	// Keeping the failed file is a convenience, so no failure here stops the
	// rollback. The state names the failed release either way.
	if err := fs.remove(failed); err == nil || os.IsNotExist(err) {
		fs.link(dst, failed)
	}
	if err := fs.rename(PrevPath(dst), dst); err != nil {
		return err
	}
	fs.syncDir(filepath.Dir(dst))
	return nil
}

func swapBackRenames(fs fsOps, dst string) error {
	failed := FailedPath(dst)
	if err := fs.remove(failed); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := fs.rename(dst, failed); err != nil {
		return err
	}
	if err := fs.rename(PrevPath(dst), dst); err != nil {
		fs.rename(failed, dst)
		return err
	}
	return nil
}

// PrevPath is where Swap keeps the binary it replaced: bitllm.prev, or
// bitllm.prev.exe on Windows.
func PrevPath(dst string) string {
	ext := filepath.Ext(dst) // ".exe" on Windows, "" elsewhere
	return strings.TrimSuffix(dst, ext) + ".prev" + ext
}

// FailedPath is where a rollback keeps the binary that failed to start:
// bitllm.failed, or bitllm.failed.exe on Windows.
func FailedPath(dst string) string {
	ext := filepath.Ext(dst)
	return strings.TrimSuffix(dst, ext) + ".failed" + ext
}

// copyDurable copies src to dst with its mode, through a temp name, so dst is
// never a partial file, and syncs it to disk before it gets its name.
func copyDurable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if err := writeSynced(out, in); err != nil {
		os.Remove(tmp)
		return err
	}
	// The umask may have narrowed the mode OpenFile asked for.
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeSynced copies r into f, syncs f and closes it.
func writeSynced(f *os.File, r io.Reader) error {
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename in dir durable where the platform can. It runs after
// the swap has happened, so a failure changes nothing a caller could act on.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return // a directory cannot be opened for syncing there
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	d.Sync()
}
