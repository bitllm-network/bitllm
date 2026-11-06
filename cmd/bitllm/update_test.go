package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bitllm-org/bitllm/internal/update"
)

// fakeBinary answers --version like a release of tag, and is large enough to
// pass as one.
func fakeBinary(tag string) []byte {
	return append([]byte("#!/bin/sh\necho bitllm "+tag+"\n#"), bytes.Repeat([]byte("x"), 1<<20)...)
}

// fakeReleases serves tag as the newest release, signed by a new trusted key
// if signed is set. hits counts every request.
func fakeReleases(t *testing.T, tag string, signed bool) (c *update.Client, hits *atomic.Int32) {
	t.Helper()
	return fakeReleasesOf(t, tag, fakeBinary(tag), signed)
}

// fakeReleasesOf is fakeReleases with a given binary.
func fakeReleasesOf(t *testing.T, tag string, bin []byte, signed bool) (c *update.Client, hits *atomic.Int32) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	asset := update.AssetName()
	if asset == "" {
		t.Skip("no release asset for this platform")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := update.TrustedKeys
	update.TrustedKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { update.TrustedKeys = old })

	sums := []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(bin), asset))
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, update.Message(tag, sums))) + "\n"
	hits = &atomic.Int32{}
	base := "/releases/download/" + tag + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		switch req.URL.Path {
		case "/latest/download/SHA256SUMS":
			http.Redirect(w, req, base+"SHA256SUMS", http.StatusFound)
		case base + "SHA256SUMS":
			w.Write(sums)
		case base + "SHA256SUMS.sig":
			if !signed {
				http.NotFound(w, req)
				return
			}
			fmt.Fprint(w, sig)
		case base + asset:
			w.Write(bin)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return &update.Client{
		Sums:             srv.URL + "/latest/download/SHA256SUMS",
		Asset:            srv.URL + "/releases/download/%s/%s",
		HTTP:             srv.Client(),
		RequireSignature: update.New().RequireSignature,
	}, hits
}

func oldBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "bitllm")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func wantBinary(t *testing.T, exe string, want []byte) {
	t.Helper()
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, want) {
		t.Errorf("%s holds %d bytes, want %d", exe, len(got), len(want))
	}
}

func wantOutput(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output is missing %q:\n%s", w, out)
		}
	}
}

// --tag can go back to an older release, and says what auto-update will do.
func TestUpdateTagInstallsOlderRelease(t *testing.T) {
	c, _ := fakeReleases(t, "v0.3.0", true)
	exe := oldBinary(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), c, updateOptions{tag: "v0.3.0", exe: exe, current: "v0.4.0"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	wantBinary(t, exe, fakeBinary("v0.3.0"))
	wantBinary(t, update.PrevPath(exe), []byte("old"))
	wantOutput(t, out.String(), "release: v0.3.0\n", "signature: ok\n", "installed v0.3.0", "--auto-update=false")
}

// --tag goes through the same checks: a binary reporting another version is
// not installed.
func TestUpdateTagChecksTheBinary(t *testing.T) {
	c, _ := fakeReleasesOf(t, "v0.3.0", fakeBinary("v0.2.0"), true)
	exe := oldBinary(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), c, updateOptions{tag: "v0.3.0", exe: exe, current: "v0.4.0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "reports") {
		t.Errorf("runUpdate = %v, want the version check to refuse it", err)
	}
	wantBinary(t, exe, []byte("old"))
}

func TestUpdateTagUnknownRelease(t *testing.T) {
	c, _ := fakeReleases(t, "v0.3.0", true)
	exe := oldBinary(t)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), c, updateOptions{tag: "v0.3.1", exe: exe, current: "v0.4.0"}, &out); err == nil {
		t.Error("installed a release that does not exist")
	}
	wantBinary(t, exe, []byte("old"))
}

func TestUpdateTagRefusesNonRelease(t *testing.T) {
	c, hits := fakeReleases(t, "v0.3.0", true)
	exe := oldBinary(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), c, updateOptions{tag: "main", exe: exe, current: "v0.4.0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not a release tag") {
		t.Errorf("--tag main = %v, want a refusal", err)
	}
	if hits.Load() != 0 {
		t.Errorf("made %d requests for a tag it should refuse", hits.Load())
	}
	wantBinary(t, exe, []byte("old"))
}

func TestUpdateTagCheckInstallsNothing(t *testing.T) {
	c, _ := fakeReleases(t, "v0.3.0", true)
	exe := oldBinary(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), c, updateOptions{check: true, tag: "v0.3.0", exe: exe, current: "v0.4.0"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	wantBinary(t, exe, []byte("old"))
	wantOutput(t, out.String(), "release: v0.3.0\n", "signature: ok\n", "this binary: v0.4.0")
	if strings.Contains(out.String(), "installed") {
		t.Errorf("--check claims an install:\n%s", out.String())
	}
}

// A release checklist relies on the exit status of --check, so any result but
// ok fails it, printed first, whatever else there is or is not to do. That
// includes the checklist's own shape: the new binary asked about its own tag.
func TestUpdateCheckFailsUnlessSigned(t *testing.T) {
	const tag = "v0.5.0"
	for _, o := range []struct {
		name         string
		tag, current string
	}{
		{"named release, this very binary", tag, tag},
		{"named release, an older binary", tag, "v0.4.6"},
		{"named release, a dev build", tag, "dev"},
		{"newest release, up to date", "", tag},
		{"newest release, an older binary", "", "v0.4.6"},
		{"newest release, a dev build", "", "dev"},
	} {
		for _, sig := range []struct {
			name, want string
			signed     bool
			otherKey   bool
		}{
			{name: "good", want: "ok", signed: true},
			{name: "missing", want: "missing"},
			{name: "wrong key", want: "bad", signed: true, otherKey: true},
		} {
			t.Run(o.name+"/"+sig.name, func(t *testing.T) {
				c, _ := fakeReleases(t, tag, sig.signed)
				if sig.otherKey {
					pub, _, err := ed25519.GenerateKey(rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
					update.TrustedKeys = []string{base64.StdEncoding.EncodeToString(pub)}
				}
				exe := oldBinary(t)
				var out bytes.Buffer
				err := runUpdate(context.Background(), c, updateOptions{check: true, tag: o.tag, exe: exe, current: o.current}, &out)
				wantOutput(t, out.String(), "signature: "+sig.want+"\n")
				if sig.want == "ok" {
					if err != nil {
						t.Errorf("runUpdate = %v, want success", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "not installing") {
					t.Errorf("runUpdate = %v, want a failure", err)
				}
				wantBinary(t, exe, []byte("old"))
			})
		}
	}
}

// --tag proves a signature without --check too: naming the release this binary
// already is must not pass just because there is nothing to install.
func TestUpdateTagThisReleaseFailsUnlessSigned(t *testing.T) {
	c, _ := fakeReleases(t, "v0.5.0", false)
	exe := oldBinary(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), c, updateOptions{tag: "v0.5.0", exe: exe, current: "v0.5.0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not installing") {
		t.Errorf("runUpdate = %v, want a failure", err)
	}
	wantOutput(t, out.String(), "signature: missing\n", "already this release")
	wantBinary(t, exe, []byte("old"))
}

// A build from source never updates itself, but the command still works on it,
// so it must enforce signatures like any other.
func TestUpdateDevBuildEnforcesSignatures(t *testing.T) {
	for _, tag := range []string{"", "v0.5.0"} {
		c, _ := fakeReleases(t, "v0.5.0", false)
		exe := oldBinary(t)
		var out bytes.Buffer
		err := runUpdate(context.Background(), c, updateOptions{tag: tag, exe: exe, current: "dev"}, &out)
		if err == nil || !strings.Contains(err.Error(), "not installing") {
			t.Errorf("tag %q: runUpdate = %v, want a refusal", tag, err)
		}
		wantBinary(t, exe, []byte("old"))

		c, _ = fakeReleases(t, "v0.5.0", true)
		if err := runUpdate(context.Background(), c, updateOptions{tag: tag, exe: exe, current: "dev"}, &out); err != nil {
			t.Errorf("tag %q: signed release: %v", tag, err)
		}
		wantBinary(t, exe, fakeBinary("v0.5.0"))
	}
}

func TestUpdateTagAlreadyThatRelease(t *testing.T) {
	c, _ := fakeReleases(t, "v0.3.0", true)
	exe := oldBinary(t)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), c, updateOptions{tag: "v0.3.0", exe: exe, current: "v0.3.0"}, &out); err != nil {
		t.Fatal(err)
	}
	wantBinary(t, exe, []byte("old"))
	wantOutput(t, out.String(), "already this release")
}

func TestUpdateTagEnforcedRefuses(t *testing.T) {
	c, _ := fakeReleases(t, "v0.3.0", false)
	exe := oldBinary(t)
	var out bytes.Buffer
	err := runUpdate(context.Background(), c, updateOptions{tag: "v0.3.0", exe: exe, current: "v0.4.0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not installing") {
		t.Errorf("runUpdate = %v, want a refusal", err)
	}
	wantBinary(t, exe, []byte("old"))
	wantOutput(t, out.String(), "signature: missing\n")
}

// Without --tag it is the newest release, the signature result is printed, and
// the note about moving forward does not apply.
func TestUpdateNewestPrintsSignature(t *testing.T) {
	c, _ := fakeReleases(t, "v0.5.0", true)
	exe := oldBinary(t)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), c, updateOptions{exe: exe, current: "v0.4.0"}, &out); err != nil {
		t.Fatal(err)
	}
	wantBinary(t, exe, fakeBinary("v0.5.0"))
	wantOutput(t, out.String(), "newest release: v0.5.0\n", "signature: ok\n", "installed v0.5.0")
	if strings.Contains(out.String(), "--auto-update") {
		t.Errorf("unexpected auto-update note:\n%s", out.String())
	}
}

// Nothing was asked of the release here and nothing is at stake, so a plain
// `bitllm update` stays quiet about an older unsigned latest.
func TestUpdateNewestUpToDate(t *testing.T) {
	c, _ := fakeReleases(t, "v0.5.0", false)
	exe := oldBinary(t)
	var out bytes.Buffer
	if err := runUpdate(context.Background(), c, updateOptions{exe: exe, current: "v0.5.0"}, &out); err != nil {
		t.Fatal(err)
	}
	wantBinary(t, exe, []byte("old"))
	wantOutput(t, out.String(), "signature: missing\n", "is up to date")
}
