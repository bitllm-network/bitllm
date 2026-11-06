package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bitllm-org/bitllm/internal/update"
)

func trust(t *testing.T, keys ...string) {
	t.Helper()
	old := update.TrustedKeys
	update.TrustedKeys = keys
	t.Cleanup(func() { update.TrustedKeys = old })
}

// runOK runs a subcommand that has to succeed and returns its stdout.
func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
	}
	return stdout.String()
}

// runFails runs a subcommand that has to exit 1 and returns its stderr.
func runFails(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("%v: exit %d, want 1; stdout %q", args, code, stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("%v: printed %q on failure", args, stdout.String())
	}
	if stderr.Len() == 0 {
		t.Errorf("%v: failed without saying why", args)
	}
	return stderr.String()
}

// newKey runs keygen and returns the key file and the printed public key.
func newKey(t *testing.T) (file, pub string) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "release.key")
	return file, strings.TrimSpace(runOK(t, "keygen", file))
}

func writeFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var testSums = []byte(strings.Repeat("a", 64) + "  bitllm-linux-amd64\n")

func TestKeygenSignVerifyRoundTrip(t *testing.T) {
	key, pub := newKey(t)
	trust(t, pub)
	sums := writeFile(t, "SHA256SUMS", testSums)

	line := runOK(t, "sign", "--key", key, "--tag", "v1.2.3", "--sums", sums)
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("sign printed %q, want one line", line)
	}
	if err := update.Verify("v1.2.3", testSums, []byte(line)); err != nil {
		t.Errorf("update.Verify rejected what sign made: %v", err)
	}
	sig := writeFile(t, "SHA256SUMS.sig", []byte(line))
	runOK(t, "verify", "--tag", "v1.2.3", "--sums", sums, "--sig", sig)
}

func TestVerifyFails(t *testing.T) {
	key, pub := newKey(t)
	otherKey, otherPub := newKey(t)
	sums := writeFile(t, "SHA256SUMS", testSums)
	sig := writeFile(t, "SHA256SUMS.sig", []byte(runOK(t, "sign", "--key", key, "--tag", "v1.2.3", "--sums", sums)))
	otherSig := writeFile(t, "SHA256SUMS.sig", []byte(runOK(t, "sign", "--key", otherKey, "--tag", "v1.2.3", "--sums", sums)))
	empty := writeFile(t, "SHA256SUMS.sig", nil)
	tampered := writeFile(t, "SHA256SUMS", append(bytes.Clone(testSums), '\n'))

	for _, tc := range []struct {
		name, tag, sums, sig string
		keys                 []string
		want                 error
	}{
		{"another tag", "v1.2.4", sums, sig, []string{pub}, update.ErrSigBad},
		{"tampered sums", "v1.2.3", tampered, sig, []string{pub}, update.ErrSigBad},
		{"wrong key", "v1.2.3", sums, otherSig, []string{pub}, update.ErrSigBad},
		{"empty file", "v1.2.3", sums, empty, []string{pub}, update.ErrSigMissing},
		{"no keys", "v1.2.3", sums, sig, nil, update.ErrNoKeys},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trust(t, tc.keys...)
			got := runFails(t, "verify", "--tag", tc.tag, "--sums", tc.sums, "--sig", tc.sig)
			if !strings.Contains(got, tc.want.Error()) {
				t.Errorf("stderr = %q, want %q", got, tc.want)
			}
		})
	}

	trust(t, pub, otherPub)
	runOK(t, "verify", "--tag", "v1.2.3", "--sums", sums, "--sig", otherSig)
	runFails(t, "verify", "--tag", "v1.2.3", "--sums", sums, "--sig", filepath.Join(t.TempDir(), "absent"))
	runFails(t, "verify", "--tag", "main", "--sums", sums, "--sig", sig)
}

// Both admins' lines in one file verify, whichever key a node trusts.
func TestTwoSignaturesInOneFile(t *testing.T) {
	keyA, pubA := newKey(t)
	keyB, pubB := newKey(t)
	sums := writeFile(t, "SHA256SUMS", testSums)
	lines := runOK(t, "sign", "--key", keyA, "--tag", "v1.2.3", "--sums", sums) +
		runOK(t, "sign", "--key", keyB, "--tag", "v1.2.3", "--sums", sums)
	sig := writeFile(t, "SHA256SUMS.sig", []byte(lines))
	for _, pub := range []string{pubA, pubB} {
		trust(t, pub)
		runOK(t, "verify", "--tag", "v1.2.3", "--sums", sums, "--sig", sig)
	}
}

func TestKeygenFile(t *testing.T) {
	key, pub := newKey(t)
	if _, err := update.ParseKey(pub); err != nil {
		t.Errorf("keygen printed %q: %v", pub, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(key)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("key file mode = %o, want 600", mode)
		}
	}
	before, _ := os.ReadFile(key)
	if strings.Contains(string(before), pub) {
		t.Error("key file holds the public key instead of the private one")
	}
	runFails(t, "keygen", key)
	after, _ := os.ReadFile(key)
	if !bytes.Equal(before, after) {
		t.Error("keygen overwrote an existing key")
	}
}

func TestSignRefuses(t *testing.T) {
	key, _ := newKey(t)
	sums := writeFile(t, "SHA256SUMS", testSums)
	for _, tag := range []string{"main", "dev", "v1.2", "v1.2.3-rc1", "1.2.3"} {
		if got := runFails(t, "sign", "--key", key, "--tag", tag, "--sums", sums); !strings.Contains(got, "not a release tag") {
			t.Errorf("sign --tag %s: %q", tag, got)
		}
	}
	notKey := writeFile(t, "release.key", []byte("not a key\n"))
	runFails(t, "sign", "--key", notKey, "--tag", "v1.2.3", "--sums", sums)
	runFails(t, "sign", "--key", key, "--tag", "v1.2.3", "--sums", filepath.Join(t.TempDir(), "absent"))
}

func TestCheckKeys(t *testing.T) {
	_, pub := newKey(t)
	short := base64.StdEncoding.EncodeToString([]byte("too short"))

	trust(t)
	if got := runFails(t, "check-keys"); !strings.Contains(got, "empty") {
		t.Errorf("no keys: %q", got)
	}
	trust(t, pub, short)
	if got := runFails(t, "check-keys"); !strings.Contains(got, short) {
		t.Errorf("short key: %q", got)
	}
	trust(t, "not base64!")
	runFails(t, "check-keys")
	trust(t, pub)
	runOK(t, "check-keys")
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"keygen"},
		{"keygen", "a", "b"},
		{"sign"},
		{"sign", "--tag", "v1.2.3"},
		{"sign", "--bogus"},
		{"verify", "--tag", "v1.2.3"},
		{"check-keys", "extra"},
	} {
		got := runFails(t, args...)
		if !strings.Contains(got, "usage:") {
			t.Errorf("%v: stderr = %q, want usage", args, got)
		}
	}
}
