package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.3.1", "v0.3.0", true},
		{"v0.10.0", "v0.9.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.2.9", "v0.3.0", false},
		{"v0.3.0", "v0.10.0", false},
		{"v0.3.0", "dev", false},
		{"v0.3.0", "v0.3", false},
		{"0.3.0", "v0.2.0", false},
		{"v0.3.0-rc1", "v0.2.0", false},
	} {
		if got := Newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	// The four targets scripts/release.sh builds. Renaming one of these
	// without updating assetName breaks self-update for that platform.
	for _, tc := range []struct{ goos, arch, want string }{
		{"linux", "amd64", "bitllm-linux-amd64"},
		{"linux", "arm64", "bitllm-linux-arm64"},
		{"darwin", "arm64", "bitllm-darwin-arm64"},
		{"windows", "amd64", "bitllm-windows-amd64.exe"},
	} {
		if got := assetName(tc.goos, tc.arch); got != tc.want {
			t.Errorf("assetName(%q, %q) = %q, want %q", tc.goos, tc.arch, got, tc.want)
		}
	}
	for _, tc := range []struct{ goos, arch string }{
		{"darwin", "amd64"}, {"windows", "arm64"}, {"linux", "386"},
	} {
		if got := assetName(tc.goos, tc.arch); got != "" {
			t.Errorf("assetName(%q, %q) = %q, want no asset", tc.goos, tc.arch, got)
		}
	}
	if got, want := AssetName(), assetName(runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("AssetName() = %q, want %q", got, want)
	}
}

func TestIsRelease(t *testing.T) {
	for _, s := range []string{"v0.3.0", "v10.20.30"} {
		if !IsRelease(s) {
			t.Errorf("IsRelease(%q) = false", s)
		}
	}
	for _, s := range []string{"dev", "ci-abc1234", "v0.3", "v0.3.0-rc1", "v0.3.0.1", "0.3.0", ""} {
		if IsRelease(s) {
			t.Errorf("IsRelease(%q) = true", s)
		}
	}
}

func TestTagFromPath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/bitllm-org/bitllm/releases/download/v0.3.0/SHA256SUMS", "v0.3.0"},
		{"/bitllm-org/bitllm/releases/download/v1.2.3/bitllm-linux-amd64", "v1.2.3"},
		{"/bitllm-org/bitllm/releases/latest/download/SHA256SUMS", ""},
		{"/bitllm-org/bitllm/releases/download/main/SHA256SUMS", ""},
		{"/nothing/here", ""},
	} {
		if got := tagFromPath(tc.path); got != tc.want {
			t.Errorf("tagFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestParseSums(t *testing.T) {
	body := "aa" + strings.Repeat("0", 62) + "  bitllm-linux-amd64\n" +
		strings.Repeat("b", 64) + " *bitllm-windows-amd64.exe\n" +
		"not a line\n" +
		strings.Repeat("c", 64) + "\n"
	sums := parseSums(body)
	if got := sums["bitllm-linux-amd64"]; got != "aa"+strings.Repeat("0", 62) {
		t.Errorf("linux sum = %q", got)
	}
	if got := sums["bitllm-windows-amd64.exe"]; got != strings.Repeat("b", 64) {
		t.Errorf("windows sum = %q", got)
	}
	if len(sums) != 2 {
		t.Errorf("parsed %d sums, want 2: %v", len(sums), sums)
	}
}

// release serves a fake release: a redirect for the checksum file, the file
// itself, its signature file if there is one, and one binary.
type release struct {
	tag       string
	asset     string
	bin       []byte
	sums      []byte
	server    *httptest.Server
	sumsHits  atomic.Int32
	assetHits atomic.Int32
}

// releaseOpts changes what the fake release serves.
type releaseOpts struct {
	// sig makes SHA256SUMS.sig from the checksum file. Nil, or nil returned,
	// is a 404.
	sig func(sums []byte) []byte
	// dropSig cuts the connection on SHA256SUMS.sig.
	dropSig bool
	// extraSums is more text in the checksum file.
	extraSums string
}

func newRelease(t *testing.T, tag string, bin []byte) *release {
	t.Helper()
	return serveRelease(t, tag, bin, releaseOpts{})
}

func serveRelease(t *testing.T, tag string, bin []byte, o releaseOpts) *release {
	t.Helper()
	r := &release{tag: tag, asset: AssetName(), bin: bin}
	if r.asset == "" {
		t.Skip("no release asset for this platform")
	}
	sum := sha256.Sum256(bin)
	r.sums = []byte(hex.EncodeToString(sum[:]) + "  " + r.asset + "\n" + o.extraSums)
	var sig []byte
	if o.sig != nil {
		sig = o.sig(r.sums)
	}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/latest/download/SHA256SUMS":
			http.Redirect(w, req, "/releases/download/"+r.tag+"/SHA256SUMS", http.StatusFound)
		case req.URL.Path == "/releases/download/"+r.tag+"/SHA256SUMS":
			r.sumsHits.Add(1)
			w.Write(r.sums)
		case req.URL.Path == "/releases/download/"+r.tag+"/SHA256SUMS.sig":
			if o.dropSig {
				panic(http.ErrAbortHandler)
			}
			if sig == nil {
				http.NotFound(w, req)
				return
			}
			w.Write(sig)
		case req.URL.Path == "/releases/download/"+r.tag+"/"+r.asset:
			r.assetHits.Add(1)
			w.Write(r.bin)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *release) client() *Client {
	return &Client{
		Sums:  r.server.URL + "/latest/download/SHA256SUMS",
		Asset: r.server.URL + "/releases/download/%s/%s",
		HTTP:  r.server.Client(),
	}
}

// script is a stand-in for a release binary: it answers --version with tag.
// Padded past minSize so the size check is exercised as it is in production.
func script(tag string) []byte {
	b := []byte("#!/bin/sh\necho bitllm " + tag + "\n#")
	return append(b, bytes.Repeat([]byte("x"), minSize)...)
}

func TestLatest(t *testing.T) {
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	rel, err := r.client().Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v9.9.9" || rel.Asset != AssetName() || len(rel.Sha256) != 64 {
		t.Fatalf("Latest = %+v", rel)
	}
}

func TestLatestNoChecksumForAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/latest/") {
			http.Redirect(w, req, "/releases/download/v1.0.0/SHA256SUMS", http.StatusFound)
			return
		}
		fmt.Fprint(w, strings.Repeat("a", 64)+"  bitllm-other-arch\n")
	}))
	defer srv.Close()
	c := &Client{Sums: srv.URL + "/latest/download/SHA256SUMS", Asset: srv.URL + "/releases/download/%s/%s", HTTP: srv.Client()}
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("Latest accepted a checksum file without our asset")
	}
}

func TestFetch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	rel := Release{Tag: r.tag, Asset: r.asset, Sha256: fmt.Sprintf("%x", sha256.Sum256(r.bin))}
	dir := t.TempDir()
	path, err := r.client().Fetch(context.Background(), rel, dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("temp file %s is not in %s", path, dir)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(r.bin) {
		t.Errorf("downloaded %q, want %q", b, r.bin)
	}
	if fi, _ := os.Stat(path); fi.Mode()&0o100 == 0 {
		t.Errorf("mode %v is not executable", fi.Mode())
	}
}

func TestFetchRejectsBadChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	rel := Release{Tag: r.tag, Asset: r.asset, Sha256: strings.Repeat("0", 64)}
	dir := t.TempDir()
	if _, err := r.client().Fetch(context.Background(), rel, dir); err == nil {
		t.Fatal("Fetch accepted a bad checksum")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestFetchRejectsWrongVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	bin := script("v0.0.1")
	r := newRelease(t, "v9.9.9", bin)
	rel := Release{Tag: r.tag, Asset: r.asset, Sha256: fmt.Sprintf("%x", sha256.Sum256(bin))}
	if _, err := r.client().Fetch(context.Background(), rel, t.TempDir()); err == nil {
		t.Fatal("Fetch accepted a binary that reports another version")
	}
}

func TestSwapKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bitllm")
	src := filepath.Join(dir, "bitllm-new")
	os.WriteFile(dst, []byte("old"), 0o755)
	os.WriteFile(src, []byte("new"), 0o755)
	if err := Swap(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("target = %q, want new", b)
	}
	if b, _ := os.ReadFile(PrevPath(dst)); string(b) != "old" {
		t.Errorf("previous = %q, want old", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still exists: %v", err)
	}
}

func TestPrevPath(t *testing.T) {
	if got := PrevPath("/home/u/.local/bin/bitllm"); got != "/home/u/.local/bin/bitllm.prev" {
		t.Errorf("PrevPath = %q", got)
	}
	if got := PrevPath(`C:\bitllm\bitllm.exe`); got != `C:\bitllm\bitllm.prev.exe` {
		t.Errorf("PrevPath(.exe) = %q", got)
	}
}

// runnerFor builds a Runner against a fake release, with target standing in
// for the running binary.
func runnerFor(t *testing.T, r *release, dir, target string) *Runner {
	t.Helper()
	return &Runner{
		Current:    "v0.3.0",
		Executable: target,
		DataDir:    dir,
		Client:     r.client(),
		Poll:       time.Millisecond,
		CacheFor:   time.Minute,
		Logf:       t.Logf,
	}
}

func TestRunnerLetsDevBuildAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Current = "dev"
	var restarted atomic.Bool
	run.Restart = func(string) error { restarted.Store(true); return nil }

	run.once(context.Background())

	if restarted.Load() {
		t.Error("a dev build restarted itself")
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Errorf("dev build changed its binary: %q", b)
	}
}

func TestRunnerUpdatesWhenIdle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)

	var calls atomic.Int32
	run.Idle = func() bool { return calls.Add(1) >= 3 }
	var restarted atomic.Bool
	run.Restart = func(path string) error {
		if path != target {
			t.Errorf("restarted into %q, want %q", path, target)
		}
		restarted.Store(true)
		return nil
	}

	run.once(context.Background())

	if !restarted.Load() {
		t.Fatal("did not restart after the node went idle")
	}
	if b, _ := os.ReadFile(target); string(b) != string(script("v9.9.9")) {
		t.Errorf("target = %q, want the new release", b)
	}
	if st := run.Status(); st["applied"] != "v9.9.9" || st["latest"] != "v9.9.9" {
		t.Errorf("status = %v", st)
	}
}

// While the node stays busy the new binary is in place and nothing restarts,
// for as long as it lasts: only the end of the node stops the wait.
func TestRunnerUpdatesButKeepsServing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	run.Idle = func() bool {
		if polls++; polls == 100 {
			cancel() // the node shuts down, still busy
		}
		return false
	}
	run.Restart = func(string) error {
		t.Error("restarted while busy")
		return nil
	}

	run.once(ctx)

	if polls < 100 {
		t.Errorf("stopped waiting after %d polls, while still busy", polls)
	}

	if b, _ := os.ReadFile(target); string(b) != string(script("v9.9.9")) {
		t.Errorf("target = %q, want the new release in place for the next start", b)
	}
	if st := run.Status(); st["pending_restart"] != true {
		t.Errorf("status = %v, want pending_restart", st)
	}
}

// The deadline on a check and download does not apply to the wait after it.
func TestRunnerBusyPastTheDeadlineRestartsOnceIdle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Timeout = 2 * time.Second
	var busySince time.Time
	sawPending := false
	run.Idle = func() bool {
		if busySince.IsZero() {
			busySince = time.Now()
		}
		if run.Status()["pending_restart"] == true {
			sawPending = true
		}
		return time.Since(busySince) > run.Timeout+200*time.Millisecond
	}
	restarts := 0
	run.Restart = func(string) error { restarts++; return nil }

	run.once(context.Background())

	if restarts != 1 {
		t.Errorf("restarted %d times, want once", restarts)
	}
	if !sawPending {
		t.Error("status did not show pending_restart during the wait")
	}
}

// Where the platform cannot restart in place the binary is installed and the
// node carries on, without waiting and without repeating itself.
func TestRunnerWithoutRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Idle = func() bool { return false }
	logs := captureLogs(run)

	run.once(context.Background())
	run.once(context.Background())

	if b, _ := os.ReadFile(target); string(b) != string(script("v9.9.9")) {
		t.Errorf("target = %q, want the new release", b)
	}
	if st := run.Status(); st["pending_restart"] != true {
		t.Errorf("status = %v, want pending_restart", st)
	}
	if n := logs.count("restart the node"); n != 1 {
		t.Errorf("logged the restart hint %d times, want once: %v", n, logs.all())
	}
	if r.assetHits.Load() != 1 {
		t.Errorf("downloaded %d times, want once", r.assetHits.Load())
	}
}

// appliedState is the state left behind after v9.9.9 was installed.
func appliedState(r *release) state {
	return state{
		Checked:   time.Now(),
		Latest:    r.tag,
		Asset:     r.asset,
		Sha256:    fmt.Sprintf("%x", sha256.Sum256(r.bin)),
		Signature: SigOK,
		Applied:   r.tag,
	}
}

// A node that was installed but never restarted picks the wait up again,
// without downloading anything.
func TestRunnerResumesWhenAlreadyApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, r.bin, 0o755)
	run := runnerFor(t, r, dir, target)
	run.saveState(appliedState(r))
	var calls atomic.Int32
	run.Idle = func() bool { return calls.Add(1) >= 3 }
	restarts := 0
	run.Restart = func(path string) error {
		if path != target {
			t.Errorf("restarted into %q, want %q", path, target)
		}
		restarts++
		return nil
	}

	run.once(context.Background())

	if restarts != 1 {
		t.Errorf("restarted %d times, want once", restarts)
	}
	if calls.Load() < 3 {
		t.Errorf("restarted after %d idle checks, without waiting for idle", calls.Load())
	}
	if r.assetHits.Load() != 0 {
		t.Errorf("downloaded %d times, want none", r.assetHits.Load())
	}
}

// A release already on disk does not need the release page to be up.
func TestRunnerResumesWithoutTheNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, r.bin, 0o755)
	run := runnerFor(t, r, dir, target)
	st := appliedState(r)
	st.Checked = time.Now().Add(-time.Hour)
	run.saveState(st)
	restarts := 0
	run.Restart = func(string) error { restarts++; return nil }
	r.server.Close()

	run.once(context.Background())

	if restarts != 1 {
		t.Errorf("restarted %d times, want once", restarts)
	}
}

// Nothing resumes once the node already runs that release, or a later one.
func TestRunnerDoesNotResumeWhenCurrent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, r.bin, 0o755)
	run := runnerFor(t, r, dir, target)
	run.Current = "v9.9.9"
	run.saveState(appliedState(r))
	run.Restart = func(string) error {
		t.Error("restarted a node that is already current")
		return nil
	}

	run.once(context.Background())

	if st := run.Status(); st["pending_restart"] != nil {
		t.Errorf("status = %v, want no pending_restart", st)
	}
}

// A binary replaced by hand is not the release the state names. Restarting
// into it would start the same check over, so it is fetched again instead.
func TestRunnerHandReplacedBinaryIsFetchedAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, script("v0.3.0"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.saveState(appliedState(r))
	restarts := 0
	run.Restart = func(path string) error {
		restarts++
		if err := runsAs(context.Background(), path, "v9.9.9"); err != nil {
			t.Errorf("restarted into the wrong binary: %v", err)
		}
		return nil
	}

	run.once(context.Background())

	if restarts != 1 {
		t.Errorf("restarted %d times, want once", restarts)
	}
	if r.assetHits.Load() != 1 {
		t.Errorf("downloaded %d times, want once", r.assetHits.Load())
	}
	if got := run.loadState().Applied; got != "v9.9.9" {
		t.Errorf("state applied = %q, want v9.9.9", got)
	}
}

// When it cannot be fetched again, the node stays where it is, with no restart
// now or at the next check.
func TestRunnerHandReplacedBinaryDoesNotLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	own := script("v0.3.0")
	os.WriteFile(target, own, 0o755)
	run := runnerFor(t, r, dir, target)
	run.saveState(appliedState(r))
	run.Restart = func(string) error {
		t.Error("restarted into a binary that is not the release")
		return nil
	}
	r.server.Close()

	run.once(context.Background())
	run.once(context.Background())

	if got := run.loadState().Applied; got != "" {
		t.Errorf("state applied = %q, want it cleared", got)
	}
	if st := run.Status(); st["applied"] != nil || st["pending_restart"] != nil {
		t.Errorf("status = %v, want no applied and no pending_restart", st)
	}
	if b, _ := os.ReadFile(target); string(b) != string(own) {
		t.Error("the binary changed")
	}
}

// A restart that fails is tried again at the next regular check, once per
// check, and not in a loop.
func TestRunnerFailedRestartWaitsForTheNextCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Idle = func() bool { return true }
	restarts := 0
	run.Restart = func(string) error { restarts++; return fmt.Errorf("exec failed") }
	logs := captureLogs(run)

	run.once(context.Background())
	if restarts != 1 {
		t.Fatalf("restarted %d times in one check, want once", restarts)
	}
	if n := logs.count("exec failed"); n != 1 {
		t.Errorf("logged the failure %d times, want once: %v", n, logs.all())
	}
	if st := run.Status(); st["pending_restart"] != true {
		t.Errorf("status = %v, want pending_restart", st)
	}

	run.once(context.Background())
	if restarts != 2 {
		t.Errorf("restarted %d times after a second check, want 2", restarts)
	}
	if r.assetHits.Load() != 1 {
		t.Errorf("downloaded %d times, want once", r.assetHits.Load())
	}
}

func TestRunnerFailedRestartDoesNotSpin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.After = time.Millisecond
	run.Every = time.Hour
	run.Idle = func() bool { return true }
	var restarts atomic.Int32
	run.Restart = func(string) error { restarts.Add(1); return fmt.Errorf("exec failed") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { run.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // Poll is 1 ms, so a spin would show here
	cancel()
	<-done

	if got := restarts.Load(); got != 1 {
		t.Errorf("restarted %d times, want once until the next check", got)
	}
}

func TestRunnerCachesTheCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.Restart = func(string) error { return nil }
	run.Idle = func() bool { return true }

	run.once(context.Background())
	before := run.Status()["checked"]
	r.server.Close() // any second request would now fail

	run.once(context.Background())

	if after := run.Status()["checked"]; after != before {
		t.Errorf("checked time changed from %v to %v despite the cache", before, after)
	}
}

// logs collects a Runner's log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func captureLogs(run *Runner) *logs {
	l := &logs{}
	run.Logf = func(format string, args ...any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, fmt.Sprintf(format, args...))
	}
	return l
}

func (l *logs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *logs) count(sub string) int {
	n := 0
	for _, line := range l.all() {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}
