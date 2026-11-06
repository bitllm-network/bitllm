package receipts

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadSeedCreatesThenKeeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipt.seed")
	a, err := LoadSeed(path)
	if err != nil || len(a) != 32 {
		t.Fatalf("first run: %d bytes, %v", len(a), err)
	}
	if st, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("stat: %v %v", st, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if b, err := LoadSeed(path); err != nil || !bytes.Equal(a, b) {
		t.Fatalf("second run changed the seed: %v", err)
	}
	// Deleting the file starts a new seed, as the doc comment says.
	os.Remove(path)
	if b, err := LoadSeed(path); err != nil || bytes.Equal(a, b) {
		t.Fatalf("after delete: %v", err)
	}
}

// A seed file of the wrong size, or one that cannot be read, stays as it is.
func TestLoadSeedKeepsAFileItCannotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.seed")
	bad := []byte("too short")
	os.WriteFile(path, bad, 0o600)
	if _, err := LoadSeed(path); err == nil {
		t.Fatal("a wrong-length seed was accepted")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatalf("the file was overwritten: %q", got)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this user")
	}
	// Write-only: reading fails and a plain write would go through.
	os.Chmod(path, 0o200)
	defer os.Chmod(path, 0o600)
	if _, err := LoadSeed(path); err == nil {
		t.Fatal("an unreadable seed was replaced")
	}
	os.Chmod(path, 0o600)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatalf("the file was overwritten: %q", got)
	}
}
