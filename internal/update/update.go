// Package update keeps a release build current: it asks GitHub which tag is
// the latest release, checks the signature on its checksum file, downloads and
// verifies the binary for this machine, and lets the node restart into it when
// nothing is being served.
//
// Only release builds take part. A build from source ("dev") or a CI build
// ("ci-abc1234") never replaces itself, so go run, go build and the artifacts
// attached to a pull request are left alone. See IsRelease.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Where releases live. The checksum file is small and its redirect chain
// ends at .../releases/download/vX.Y.Z/SHA256SUMS, which is how the tag is
// read without the rate-limited API and without a token.
const (
	repo         = "bitllm-org/bitllm"
	DefaultSums  = "https://github.com/" + repo + "/releases/latest/download/SHA256SUMS"
	DefaultAsset = "https://github.com/" + repo + "/releases/download/%s/%s"

	// A release binary is tens of MB. These bounds catch an HTML error page
	// or a truncated download before it is ever executed.
	minSize = 1 << 20
	maxSize = 200 << 20

	// maxSumsSize bounds SHA256SUMS.
	maxSumsSize = 1 << 16
)

// Client talks to the release page. The fields exist so tests can point at an
// httptest server; New fills in the real ones.
type Client struct {
	Sums  string
	Asset string // fmt with tag, asset name
	HTTP  *http.Client

	// RequireSignature makes Fetch refuse a release whose Signature is not
	// SigOK. Off, the result is only reported. New sets it from
	// requireSignature.
	RequireSignature bool
}

// New is the client for the public release page. There is no overall
// timeout: a 27 MB binary on a slow link takes minutes, and the caller's
// context bounds the download. The transport still gives up on a peer that
// never answers.
func New() *Client {
	return &Client{
		Sums:  DefaultSums,
		Asset: DefaultAsset,
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}},
		RequireSignature: requireSignature,
	}
}

// Release is one release as it applies to this machine.
type Release struct {
	Tag       string `json:"tag"`       // v0.3.1
	Asset     string `json:"asset"`     // bitllm-linux-amd64
	Sha256    string `json:"sha256"`    // hex, from the release's SHA256SUMS
	Signature string `json:"signature"` // SigOK, SigMissing, SigBad or SigNoKeys
}

// Latest reads the newest release's checksum file. The tag comes from the
// final download URL, the checksum for this machine from the file itself.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	hc := *c.HTTP
	var tag string
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		if t := tagFromPath(req.URL.Path); t != "" {
			tag = t
		}
		return nil
	}
	sums, final, err := getSums(ctx, &hc, c.Sums)
	if err != nil {
		return Release{}, fmt.Errorf("latest release: %w", err)
	}
	if tag == "" {
		tag = tagFromPath(final)
	}
	if tag == "" {
		return Release{}, errors.New("latest release: no version in the download URL")
	}
	return c.release(ctx, tag, sums)
}

// Tag reads the checksum file of one named release. That reaches what Latest
// does not, such as a prerelease or an older version; everything after this
// point is the same for both.
func (c *Client) Tag(ctx context.Context, tag string) (Release, error) {
	if !IsRelease(tag) {
		return Release{}, fmt.Errorf("%q is not a release tag, want vX.Y.Z", tag)
	}
	sums, _, err := getSums(ctx, c.HTTP, fmt.Sprintf(c.Asset, tag, "SHA256SUMS"))
	if err != nil {
		return Release{}, fmt.Errorf("release %s: %w", tag, err)
	}
	return c.release(ctx, tag, sums)
}

// getSums reads a checksum file and returns its raw bytes and the path it was
// finally served from.
func getSums(ctx context.Context, hc *http.Client, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSumsSize))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Request.URL.Path, nil
}

// release finds this machine's checksum in sums and checks the signature on
// those same bytes.
func (c *Client) release(ctx context.Context, tag string, sums []byte) (Release, error) {
	asset := AssetName()
	if asset == "" {
		return Release{}, fmt.Errorf("no release binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sum, ok := parseSums(string(sums))[asset]
	if !ok {
		return Release{}, fmt.Errorf("release %s has no checksum for %s", tag, asset)
	}
	sig := sigResult(Verify(tag, sums, c.getSig(ctx, tag)))
	return Release{Tag: tag, Asset: asset, Sha256: sum, Signature: sig}, nil
}

// getSig reads the release's SHA256SUMS.sig. Any failure to get it, a 404 or a
// network error, counts as no signature, which is nil here: what that means is
// up to RequireSignature, not to this read.
func (c *Client) getSig(ctx context.Context, tag string) []byte {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(c.Asset, tag, "SHA256SUMS.sig"), nil)
	if err != nil {
		return nil
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSigSize))
	if err != nil {
		return nil
	}
	return body
}

// refusal is why a release with this signature result may not be installed;
// nil when it may.
func (c *Client) refusal(tag, sig string) error {
	if !c.RequireSignature || sig == SigOK {
		return nil
	}
	return fmt.Errorf("not installing %s: %s", tag, sigProblem(sig))
}

// Fetch downloads the release binary into dir, verifies the checksum, and
// runs it once to check the version it reports. The returned path is a temp
// file in dir, ready for Swap. With RequireSignature it refuses a release that
// is not signed, before downloading anything.
func (c *Client) Fetch(ctx context.Context, rel Release, dir string) (string, error) {
	if err := c.refusal(rel.Tag, rel.Signature); err != nil {
		return "", err
	}
	url := fmt.Sprintf(c.Asset, rel.Tag, rel.Asset)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", rel.Asset, resp.Status)
	}
	f, err := os.CreateTemp(dir, ".bitllm-update-")
	if err != nil {
		return "", err
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return fail(err)
	}
	if n < minSize || n > maxSize {
		return fail(fmt.Errorf("%s: %d bytes, not a release binary", rel.Asset, n))
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, rel.Sha256) {
		return fail(fmt.Errorf("%s: sha256 %s, want %s", rel.Asset, got, rel.Sha256))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := os.Chmod(path, 0o755); err != nil {
		os.Remove(path)
		return "", err
	}
	// The strongest cheap check: the file has to run here and report the
	// version it claims. This catches a truncated file, the wrong asset for
	// this platform, and on macOS a binary the kernel refuses to sign off.
	if err := runsAs(ctx, path, rel.Tag); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// runsAs runs path --version and requires it to name tag.
func runsAs(ctx context.Context, path, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	if got := strings.TrimSpace(string(out)); got != "bitllm "+tag {
		return fmt.Errorf("%s reports %q, want %q", filepath.Base(path), got, "bitllm "+tag)
	}
	return nil
}

// Swap puts src in place of dst. The binary it replaces is kept next to it
// (see PrevPath), so one rename goes back a version. On Windows the running
// .exe can be renamed but not overwritten, so callers there expect the new
// file to be used at the next start.
func Swap(src, dst string) error {
	prev := PrevPath(dst)
	if err := os.Remove(prev); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, prev); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err != nil {
		if _, e := os.Stat(prev); e == nil {
			os.Rename(prev, dst)
		}
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

// AssetName is the release asset for this machine, named as
// scripts/release.sh builds it. Empty when there is no build for it.
func AssetName() string {
	return assetName(runtime.GOOS, runtime.GOARCH)
}

func assetName(goos, arch string) string {
	switch goos + "/" + arch {
	case "darwin/arm64", "linux/amd64", "linux/arm64":
		return "bitllm-" + goos + "-" + arch
	case "windows/amd64":
		return "bitllm-windows-amd64.exe"
	}
	return ""
}

var versionRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Version is a release version, vMAJOR.MINOR.PATCH.
type Version [3]int

// Parse reads a release version. Anything else (dev, ci-abc1234, v0.3,
// v0.3.0-rc1) is not a release version.
func Parse(s string) (Version, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n < 0 {
			return Version{}, false
		}
		v[i] = n
	}
	return v, true
}

// IsRelease reports whether s is a version this package may act on. A build
// from source or from CI must never replace itself.
func IsRelease(s string) bool {
	_, ok := Parse(s)
	return ok
}

// Newer reports whether latest is a later release than current, in numeric
// order (v0.10.0 is newer than v0.9.9). False if either is not a release
// version.
func Newer(latest, current string) bool {
	a, ok := Parse(latest)
	if !ok {
		return false
	}
	b, ok := Parse(current)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// tagFromPath returns the version in .../releases/download/vX.Y.Z/NAME.
func tagFromPath(p string) string {
	const marker = "/releases/download/"
	i := strings.Index(p, marker)
	if i < 0 {
		return ""
	}
	rest := p[i+len(marker):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		rest = rest[:j]
	}
	tag, err := url.PathUnescape(rest)
	if err != nil || !IsRelease(tag) {
		return ""
	}
	return tag
}

// parseSums reads SHA256SUMS: one "hex  name" per line, as sha256sum writes
// it, with an optional * before binary names.
func parseSums(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			continue
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			continue
		}
		out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	return out
}

// Runner checks for a newer release and installs it, then restarts the node
// once it is idle. Run it in a goroutine.
type Runner struct {
	Current    string // node.Version, e.g. v0.3.0
	Executable string // absolute path of the running binary
	DataDir    string // where the check time is remembered
	Client     *Client

	// Idle reports that the node serves nothing: no running job, no waiter.
	Idle func() bool
	// Restart replaces the running process with path. Nil where the platform
	// cannot do it (Windows): the new binary is then used at the next start.
	Restart func(path string) error
	Logf    func(format string, args ...any)

	After    time.Duration // first check after start; 0 = 30s
	Every    time.Duration // between checks; 0 = 6h
	Poll     time.Duration // idle poll interval; 0 = 5s
	CacheFor time.Duration // skip the network if checked recently; 0 = 10m
	Timeout  time.Duration // one check and download, not the wait after it; 0 = 30m

	warned string // the tag whose signature result was last logged; only the Run goroutine uses it

	mu        sync.Mutex
	checked   time.Time
	latest    string
	applied   string
	signature string
	pending   bool
}

// Status is what GET /status shows about updates. Empty before the first
// check, and on builds that never check.
func (r *Runner) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{}
	if !r.checked.IsZero() {
		out["checked"] = r.checked.UTC().Format(time.RFC3339)
	}
	if r.latest != "" {
		out["latest"] = r.latest
	}
	if r.applied != "" {
		out["applied"] = r.applied
	}
	if r.signature != "" {
		out["signature"] = r.signature
	}
	if r.pending {
		out["pending_restart"] = true
	}
	return out
}

// publish makes st what Status reports.
func (r *Runner) publish(st state) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked, r.latest, r.applied, r.signature = st.Checked, st.Latest, st.Applied, st.Signature
}

func (r *Runner) setPending(pending bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = pending
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf == nil {
		log.Printf(format, args...)
		return
	}
	r.Logf(format, args...)
}

func (r *Runner) after() time.Duration {
	if r.After > 0 {
		return r.After
	}
	return 30 * time.Second
}

func (r *Runner) every() time.Duration {
	if r.Every > 0 {
		return r.Every
	}
	return 6 * time.Hour
}

func (r *Runner) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return 5 * time.Second
}

func (r *Runner) cacheFor() time.Duration {
	if r.CacheFor > 0 {
		return r.CacheFor
	}
	return 10 * time.Minute
}

func (r *Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 30 * time.Minute
}

// Run checks now and then every Every, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	if r.Client == nil {
		r.Client = New()
	}
	t := time.NewTimer(jitter(r.after()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.once(ctx)
		t.Reset(jitter(r.every()))
	}
}

// jitter spreads checks over ±10 %, so a release does not wake every node in
// the network at the same second.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := d / 10
	return d - spread + rand.N(2*spread+1)
}

// once does one check and, if there is a newer release, installs it and
// restarts when the node is idle. Any failure leaves the running binary alone
// and is logged once.
func (r *Runner) once(ctx context.Context) {
	if !IsRelease(r.Current) {
		return
	}
	if tag := r.install(ctx); tag != "" {
		r.restartWhenIdle(ctx, tag)
	}
}

// install brings the file on disk to the newest release and returns the tag a
// restart would move this node to, or "" when there is none. A slow link may
// need minutes for the binary; a stuck one must not pin this goroutine for the
// life of the node, so this phase has a deadline.
func (r *Runner) install(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	st := r.loadState()
	checkFailed := false
	// A state file from a build older than signatures has a tag and no result
	// for it, so it cannot stand in for a check.
	if st.Checked.IsZero() || time.Since(st.Checked) >= r.cacheFor() || (st.Latest != "" && st.Signature == "") {
		rel, err := r.Client.Latest(ctx)
		st.Checked = time.Now()
		if err != nil {
			checkFailed = true
			r.logf("update: %v", err)
		} else {
			st.Latest, st.Asset, st.Sha256, st.Signature = rel.Tag, rel.Asset, rel.Sha256, rel.Signature
			if Newer(rel.Tag, r.Current) {
				r.logf("update: %s is available (running %s)", rel.Tag, r.Current)
			}
		}
		r.saveState(st)
	}
	r.publish(st)
	if st.Latest == "" || !Newer(st.Latest, r.Current) {
		return ""
	}
	// Without a fresh answer nothing new is downloaded, but a release already
	// on disk needs no network.
	if checkFailed && st.Applied != st.Latest {
		return ""
	}
	if !r.signatureAllows(st.Latest, st.Signature) {
		return ""
	}
	if st.Applied == st.Latest {
		// Restart only into a file that really is this release. Otherwise the
		// new process would see the same state and restart again.
		err := runsAs(ctx, r.Executable, st.Latest)
		if err == nil {
			if r.Restart != nil {
				r.logf("update: %s is installed, restarting when idle", st.Latest)
			}
			return st.Latest
		}
		if ctx.Err() != nil {
			return ""
		}
		r.logf("update: the installed binary is not %s (%v), installing it again", st.Latest, err)
		st.Applied = ""
		r.saveState(st)
		r.publish(st)
		r.setPending(false)
		if checkFailed {
			return ""
		}
	}

	rel := Release{Tag: st.Latest, Asset: st.Asset, Sha256: st.Sha256, Signature: st.Signature}
	path, err := r.Client.Fetch(ctx, rel, filepath.Dir(r.Executable))
	if err != nil {
		r.logf("update: %v", err)
		return ""
	}
	if err := Swap(path, r.Executable); err != nil {
		os.Remove(path)
		r.logf("update: cannot replace %s: %v", r.Executable, err)
		return ""
	}
	st.Applied = st.Latest
	r.saveState(st)
	r.publish(st)
	if r.Restart == nil {
		r.logf("update: %s installed, restart the node to use it", st.Latest)
	} else {
		r.logf("update: %s installed, restarting when idle", st.Latest)
	}
	return st.Latest
}

// signatureAllows reports whether a release with this signature result may
// go ahead. Anything but SigOK is logged once per tag.
func (r *Runner) signatureAllows(tag, sig string) bool {
	if sig == SigOK {
		return true
	}
	err := r.Client.refusal(tag, sig)
	if r.warned != tag {
		r.warned = tag
		if err != nil {
			r.logf("update: %v", err)
		} else {
			r.logf("update: %s: %s; going ahead, since signatures are not required yet", tag, sigProblem(sig))
		}
	}
	return err == nil
}

// restartWhenIdle waits as long as it takes for the node to serve nothing,
// until ctx ends, then restarts into the installed binary. A failed restart
// is logged and left for the next regular check to try again.
func (r *Runner) restartWhenIdle(ctx context.Context, tag string) {
	r.setPending(true)
	if r.Restart == nil || !r.waitForIdle(ctx) {
		return
	}
	if err := r.Restart(r.Executable); err != nil {
		r.logf("update: restart: %v (trying again at the next check, the next start uses %s)", err, tag)
	}
}

// waitForIdle polls until the node serves nothing. False means ctx ended.
func (r *Runner) waitForIdle(ctx context.Context) bool {
	for ctx.Err() == nil {
		if r.Idle == nil || r.Idle() {
			return true
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.poll()):
		}
	}
	return false
}

// state is what survives a restart: when we last looked, what we saw, and
// whether the file on disk is already that version.
type state struct {
	Checked   time.Time `json:"checked,omitempty"`
	Latest    string    `json:"latest,omitempty"`
	Asset     string    `json:"asset,omitempty"`
	Sha256    string    `json:"sha256,omitempty"`
	Signature string    `json:"signature,omitempty"`
	Applied   string    `json:"applied,omitempty"`
}

func (r *Runner) statePath() string {
	if r.DataDir == "" {
		return ""
	}
	return filepath.Join(r.DataDir, "update.json")
}

func (r *Runner) loadState() state {
	var st state
	if p := r.statePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &st)
		}
	}
	return st
}

func (r *Runner) saveState(st state) {
	p := r.statePath()
	if p == "" {
		return
	}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
	}
}
