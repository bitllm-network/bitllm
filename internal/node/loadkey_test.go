package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadKeyCreatesThenKeeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.key")
	a, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatalf("stat: %v %v", st, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	b, err := LoadKey(path)
	if err != nil || !a.Equals(b) {
		t.Fatalf("second run changed the key: %v", err)
	}
}

// A key that does not parse is an error that says what to do, and stays.
func TestLoadKeyKeepsADamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")
	bad := []byte("not a key")
	os.WriteFile(path, bad, 0o600)
	_, err := LoadKey(path)
	if err == nil || !strings.Contains(err.Error(), "restore it from a backup") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatalf("the file was overwritten: %q", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := New(ctx, Config{DataDir: filepath.Dir(path), AnnounceEvery: time.Second}); err == nil {
		t.Fatal("a node started with a damaged identity")
	}
}

// A key that cannot be read is never replaced: today's peer id is the node.
func TestLoadKeyKeepsAnUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this user")
	}
	path := filepath.Join(t.TempDir(), "identity.key")
	k, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write-only: reading fails and a plain write would go through.
	os.Chmod(path, 0o200)
	defer os.Chmod(path, 0o600)
	if _, err := LoadKey(path); err == nil || !strings.Contains(err.Error(), "has not been replaced") {
		t.Fatalf("err = %v", err)
	}
	os.Chmod(path, 0o600)
	again, err := LoadKey(path)
	if err != nil || !k.Equals(again) {
		t.Fatalf("the identity changed: %v", err)
	}
}

// The file appears in one step: a rename, not a write into the final name.
func TestWriteFileAtomicReplacesInOneStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	os.WriteFile(path, []byte("old"), 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Link(path, link); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if err := writeFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("file = %q", got)
	}
	if got, _ := os.ReadFile(link); string(got) != "old" {
		t.Fatalf("the old file was written in place: %q", got)
	}
}

// A bad seed file costs this run its pseudonyms, not its start, and stays.
func TestNewStartsWithABadSeedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipt.seed")
	bad := []byte("short")
	os.WriteFile(path, bad, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := New(ctx, Config{DataDir: dir, Listen: []string{"/ip4/127.0.0.1/tcp/0"}, DHTServer: true, AnnounceEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer n.host.Close()
	if len(n.receiptSeed) != 32 || bytes.Equal(n.receiptSeed, make([]byte, 32)) {
		t.Fatalf("seed = %x", n.receiptSeed)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatalf("the seed file was overwritten: %q", got)
	}
}
