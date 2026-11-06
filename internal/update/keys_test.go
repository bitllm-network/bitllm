package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// signer is a test admin: a key pair, and signature lines made with it.
type signer struct {
	pub  string
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{pub: base64.StdEncoding.EncodeToString(pub), priv: priv}
}

// line signs tag and sums the way scripts/sign does, as one line of
// SHA256SUMS.sig.
func (s signer) line(tag string, sums []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, Message(tag, sums))) + "\n")
}

// trust sets TrustedKeys for one test.
func trust(t *testing.T, keys ...string) {
	t.Helper()
	old := TrustedKeys
	TrustedKeys = keys
	t.Cleanup(func() { TrustedKeys = old })
}

// The signed format is shared by the signing tool and every node, so it is
// spelled out here rather than taken from Message.
func TestSignedMessageFormat(t *testing.T) {
	a := newSigner(t)
	trust(t, a.pub)
	sums := []byte("abc  bitllm-linux-amd64\n")
	raw := ed25519.Sign(a.priv, []byte("bitllm release v1\nv1.2.3\nabc  bitllm-linux-amd64\n"))
	line := base64.StdEncoding.EncodeToString(raw)
	if err := Verify("v1.2.3", sums, []byte(line)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerify(t *testing.T) {
	a, b, c := newSigner(t), newSigner(t), newSigner(t)
	const tag = "v1.2.3"
	sums := []byte(strings.Repeat("a", 64) + "  bitllm-linux-amd64\n")
	good, other := a.line(tag, sums), b.line(tag, sums)
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	for _, tc := range []struct {
		name string
		keys []string
		sums []byte // nil: the sums that were signed
		sig  []byte
		want error
	}{
		{name: "good signature", keys: []string{a.pub}, sig: good},
		{name: "no trailing newline", keys: []string{a.pub}, sig: bytes.TrimSpace(good)},
		{name: "first of two keys", keys: []string{a.pub, b.pub}, sig: good},
		{name: "second of two keys", keys: []string{a.pub, b.pub}, sig: other},
		{name: "two lines, both trusted", keys: []string{a.pub, b.pub}, sig: join(good, other)},
		{name: "two lines, only the second trusted", keys: []string{b.pub}, sig: join(good, other)},
		{name: "junk line before a good one", keys: []string{a.pub}, sig: join([]byte("junk\n"), good)},
		{name: "blank lines and CRLF", keys: []string{a.pub}, sig: join([]byte("\n\r\n  \n"), bytes.TrimSpace(good), []byte("\r\n\n"))},
		{name: "a bad key entry next to a good one", keys: []string{"nope", a.pub}, sig: good},

		{name: "no file", keys: []string{a.pub}, sig: nil, want: ErrSigMissing},
		{name: "empty file", keys: []string{a.pub}, sig: []byte{}, want: ErrSigMissing},
		{name: "blank lines only", keys: []string{a.pub}, sig: []byte("\n \r\n\n"), want: ErrSigMissing},

		{name: "wrong key", keys: []string{c.pub}, sig: good, want: ErrSigBad},
		{name: "two wrong lines", keys: []string{c.pub}, sig: join(good, other), want: ErrSigBad},
		{name: "signature for another tag", keys: []string{a.pub}, sig: a.line("v1.2.2", sums), want: ErrSigBad},
		{name: "tampered sums", keys: []string{a.pub}, sums: append(bytes.Clone(sums), '\n'), sig: good, want: ErrSigBad},
		{name: "not base64", keys: []string{a.pub}, sig: []byte("<html>not found</html>\n"), want: ErrSigBad},
		{name: "wrong length", keys: []string{a.pub}, sig: []byte("AAAA\n"), want: ErrSigBad},
		{name: "good line past the size limit", keys: []string{a.pub}, sig: join(bytes.Repeat([]byte("junk\n"), maxSigSize/5+1), good), want: ErrSigBad},

		{name: "no keys", keys: nil, sig: good, want: ErrNoKeys},
		{name: "no keys and no file", keys: nil, sig: nil, want: ErrNoKeys},
		{name: "only unusable keys", keys: []string{"nope", "AAAA"}, sig: good, want: ErrNoKeys},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trust(t, tc.keys...)
			useSums := tc.sums
			if useSums == nil {
				useSums = sums
			}
			if err := Verify(tag, useSums, tc.sig); !errors.Is(err, tc.want) {
				t.Errorf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseKey(t *testing.T) {
	a := newSigner(t)
	if _, err := ParseKey(a.pub); err != nil {
		t.Errorf("ParseKey(valid) = %v", err)
	}
	for _, s := range []string{"", "AAAA", "not base64!", a.pub + "AAAA", " " + a.pub} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) accepted it", s)
		}
	}
}

// New takes its default from the release-wide switch.
func TestNewRequireSignatureDefault(t *testing.T) {
	if got := New().RequireSignature; got != requireSignature {
		t.Errorf("New().RequireSignature = %v, want %v", got, requireSignature)
	}
}

// signed returns release options with a's signature over the served file.
func signed(a signer, tag string) releaseOpts {
	return releaseOpts{sig: func(sums []byte) []byte { return a.line(tag, sums) }}
}

// Latest checks the bytes it read, whatever else is in the file.
func TestLatestSignature(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	const tag = "v9.9.9"
	for _, tc := range []struct {
		name string
		keys []string
		opts releaseOpts
		want string
	}{
		{"good", []string{a.pub}, signed(a, tag), SigOK},
		{"either key", []string{b.pub, a.pub}, signed(a, tag), SigOK},
		{"both signatures in one file", []string{b.pub}, releaseOpts{sig: func(sums []byte) []byte {
			return append(a.line(tag, sums), b.line(tag, sums)...)
		}}, SigOK},
		{"text parseSums skips is still signed", []string{a.pub}, releaseOpts{
			sig:       signed(a, tag).sig,
			extraSums: "# built from main\r\n\r\n",
		}, SigOK},
		{"404", []string{a.pub}, releaseOpts{}, SigMissing},
		{"connection cut", []string{a.pub}, releaseOpts{dropSig: true}, SigMissing},
		{"wrong key", []string{b.pub}, signed(a, tag), SigBad},
		{"another tag", []string{a.pub}, signed(a, "v9.9.8"), SigBad},
		{"tampered sums", []string{a.pub}, releaseOpts{sig: func(sums []byte) []byte {
			return a.line(tag, append(bytes.Clone(sums), '\n'))
		}}, SigBad},
		{"no keys", nil, signed(a, tag), SigNoKeys},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trust(t, tc.keys...)
			r := serveRelease(t, tag, script(tag), tc.opts)
			rel, err := r.client().Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if rel.Signature != tc.want {
				t.Errorf("Signature = %q, want %q", rel.Signature, tc.want)
			}
			if rel.Tag != tag || len(rel.Sha256) != 64 {
				t.Errorf("Latest = %+v", rel)
			}
		})
	}
}

// failingSignatures is every way a release can fail the check, for one tag.
func failingSignatures(tag string, a, b signer) []struct {
	name string
	keys []string
	opts releaseOpts
	want string
} {
	return []struct {
		name string
		keys []string
		opts releaseOpts
		want string
	}{
		{"missing", []string{a.pub}, releaseOpts{}, SigMissing},
		{"connection cut", []string{a.pub}, releaseOpts{dropSig: true}, SigMissing},
		{"wrong key", []string{b.pub}, signed(a, tag), SigBad},
		{"another tag", []string{a.pub}, signed(a, "v9.9.8"), SigBad},
		{"tampered sums", []string{a.pub}, releaseOpts{sig: func(sums []byte) []byte {
			return a.line(tag, append(bytes.Clone(sums), '\n'))
		}}, SigBad},
		{"not a signature", []string{a.pub}, releaseOpts{sig: func([]byte) []byte { return []byte("<html>\n") }}, SigBad},
		{"no keys", nil, signed(a, tag), SigNoKeys},
	}
}

// With signatures not required, a failed check is reported and logged once, and
// the update goes ahead.
func TestRunnerWarnModeInstalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	const tag = "v9.9.9"
	for _, tc := range failingSignatures(tag, newSigner(t), newSigner(t)) {
		t.Run(tc.name, func(t *testing.T) {
			trust(t, tc.keys...)
			r := serveRelease(t, tag, script(tag), tc.opts)
			dir := t.TempDir()
			target := filepath.Join(dir, "bitllm")
			os.WriteFile(target, []byte("old"), 0o755)
			run := runnerFor(t, r, dir, target)
			restarts := 0
			run.Restart = func(string) error { restarts++; return nil }
			logs := captureLogs(run)

			run.once(context.Background())
			run.CacheFor = 1 // the next check goes to the network again
			run.once(context.Background())

			if b, _ := os.ReadFile(target); string(b) != string(r.bin) {
				t.Error("warn mode did not install the release")
			}
			if restarts == 0 {
				t.Error("warn mode did not restart")
			}
			if got := run.Status()["signature"]; got != tc.want {
				t.Errorf("status signature = %v, want %q", got, tc.want)
			}
			if n := logs.count("going ahead"); n != 1 {
				t.Errorf("logged the warning %d times, want once: %v", n, logs.all())
			}
		})
	}
}

// With signatures required, every failed check leaves the running binary alone:
// nothing is downloaded, swapped or restarted.
func TestRunnerEnforcedRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	const tag = "v9.9.9"
	for _, tc := range failingSignatures(tag, newSigner(t), newSigner(t)) {
		t.Run(tc.name, func(t *testing.T) {
			trust(t, tc.keys...)
			r := serveRelease(t, tag, script(tag), tc.opts)
			dir := t.TempDir()
			target := filepath.Join(dir, "bitllm")
			os.WriteFile(target, []byte("old"), 0o755)
			run := runnerFor(t, r, dir, target)
			run.Client.RequireSignature = true
			run.Restart = func(string) error {
				t.Error("restarted into a refused release")
				return nil
			}
			logs := captureLogs(run)

			run.once(context.Background())
			run.once(context.Background())

			if b, _ := os.ReadFile(target); string(b) != "old" {
				t.Error("the running binary changed")
			}
			if r.assetHits.Load() != 0 {
				t.Errorf("downloaded the binary %d times", r.assetHits.Load())
			}
			if _, err := os.Stat(PrevPath(target)); !os.IsNotExist(err) {
				t.Errorf("a previous binary appeared: %v", err)
			}
			st := run.Status()
			if st["signature"] != tc.want || st["applied"] != nil || st["pending_restart"] != nil {
				t.Errorf("status = %v, want signature %q and nothing applied", st, tc.want)
			}
			if n := logs.count("not installing"); n != 1 {
				t.Errorf("logged the refusal %d times, want once: %v", n, logs.all())
			}
		})
	}
}

func TestRunnerEnforcedInstallsSigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	const tag = "v9.9.9"
	a := newSigner(t)
	trust(t, a.pub)
	r := serveRelease(t, tag, script(tag), signed(a, tag))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Client.RequireSignature = true
	restarts := 0
	run.Restart = func(string) error { restarts++; return nil }
	logs := captureLogs(run)

	run.once(context.Background())

	if b, _ := os.ReadFile(target); string(b) != string(r.bin) {
		t.Error("a signed release was not installed")
	}
	if restarts != 1 {
		t.Errorf("restarted %d times, want once", restarts)
	}
	if got := run.Status()["signature"]; got != SigOK {
		t.Errorf("status signature = %v, want ok", got)
	}
	if n := logs.count("signature"); n != 0 {
		t.Errorf("logged about a good signature: %v", logs.all())
	}
}

// Fetch itself refuses, since `bitllm update` calls it directly.
func TestFetchEnforcedRefuses(t *testing.T) {
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	c := r.client()
	c.RequireSignature = true
	for _, sig := range []string{SigMissing, SigBad, SigNoKeys, ""} {
		rel := Release{Tag: r.tag, Asset: r.asset, Sha256: strings.Repeat("0", 64), Signature: sig}
		if _, err := c.Fetch(context.Background(), rel, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not installing") {
			t.Errorf("Fetch with signature %q = %v, want a refusal", sig, err)
		}
	}
	if r.assetHits.Load() != 0 {
		t.Errorf("downloaded the binary %d times", r.assetHits.Load())
	}
}

// The result is kept with the check, so a cached check and a new process still
// report it.
func TestRunnerSignatureSurvivesCachedCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	const tag = "v9.9.9"
	a := newSigner(t)
	trust(t, a.pub)
	r := serveRelease(t, tag, script(tag), signed(a, tag))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Restart = func(string) error { return nil }

	run.once(context.Background())
	r.server.Close() // any second request would now fail

	next := runnerFor(t, r, dir, target)
	next.Restart = func(string) error { return nil }
	next.once(context.Background())

	if got := next.Status()["signature"]; got != SigOK {
		t.Errorf("status signature after a cached check = %v, want ok", got)
	}
	if got := next.loadState().Signature; got != SigOK {
		t.Errorf("state signature = %q, want ok", got)
	}
}

// A state file from an older build names a tag with no signature result, so it
// cannot stand in for a check, however recent it is.
func TestRunnerRechecksStateWithoutSignature(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	const tag = "v9.9.9"
	a := newSigner(t)
	trust(t, a.pub)
	r := serveRelease(t, tag, script(tag), signed(a, tag))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Restart = func(string) error { return nil }
	st := appliedState(r)
	st.Signature, st.Applied = "", ""
	run.saveState(st)

	run.once(context.Background())

	if got := run.Status()["signature"]; got != SigOK {
		t.Errorf("status signature = %v, want ok", got)
	}
	if r.sumsHits.Load() != 1 {
		t.Errorf("read the checksum file %d times, want once", r.sumsHits.Load())
	}
}
