// Package update keeps a release build current: it asks GitHub which tag is
// the latest release, checks the signature on its checksum file, downloads and
// verifies the binary for this machine, and lets the node restart into it when
// nothing is being served. A release that then fails to start is swapped back
// by the next start; see StartCheck.
//
// Only release builds take part. A build from source ("dev") or a CI build
// ("ci-abc1234") never replaces itself, so go run, go build and the artifacts
// attached to a pull request are left alone. See IsRelease.
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"slices"
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
	// SigOK. New sets it from requireSignature, so every build enforces it. Off,
	// as only a Client built by hand can be, the result is just reported.
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

// Refusal is why a release with this signature result may not be installed;
// nil when it may.
func (c *Client) Refusal(tag, sig string) error {
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
	if err := c.Refusal(rel.Tag, rel.Signature); err != nil {
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

	// RolledBackFrom is the release the last start went back from, as StartCheck
	// returned it. Status shows it.
	RolledBackFrom string

	After        time.Duration // first check after start; 0 = 30s
	Every        time.Duration // between checks; 0 = 6h
	Poll         time.Duration // idle poll interval; 0 = 5s
	MaxIdleWait  time.Duration // longest wait for idle before restarting anyway; 0 = 24h
	CacheFor     time.Duration // skip the network if checked recently; 0 = 10m
	Timeout      time.Duration // one check and download, not the wait after it; 0 = 30m
	HealthyAfter time.Duration // how long a start must last to count as healthy; 0 = 60s

	warned     string // the tag whose signature result was last logged; only the Run goroutine uses it
	skipLogged string // the tag whose skip was last logged; only the Run goroutine uses it

	stateMu sync.Mutex // held for each read, change and write of the state file

	mu        sync.Mutex
	checked   time.Time
	latest    string
	applied   string
	signature string
	pending   bool
}

// Status is what GET /status shows about updates. Empty before the first
// check, unless the last start rolled back, and on builds that never check.
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
	if r.RolledBackFrom != "" {
		out["rolled_back_from"] = r.RolledBackFrom
	}
	return out
}

// publish makes st what Status reports.
func (r *Runner) publish(st state) {
	// After a rollback Applied still names the release that failed, which is not
	// the file on disk. See starter.rollBack.
	applied := st.Applied
	if slices.Contains(st.Skip, applied) {
		applied = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked, r.latest, r.applied, r.signature = st.Checked, st.Latest, applied, st.Signature
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

func (r *Runner) maxIdleWait() time.Duration {
	if r.MaxIdleWait > 0 {
		return r.MaxIdleWait
	}
	return 24 * time.Hour
}

func (r *Runner) healthyAfter() time.Duration {
	if r.HealthyAfter > 0 {
		return r.HealthyAfter
	}
	return 60 * time.Second
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
			if Newer(rel.Tag, r.Current) && !slices.Contains(st.Skip, rel.Tag) {
				r.logf("update: %s is available (running %s)", rel.Tag, r.Current)
			}
		}
		r.saveCheck(st)
	}
	r.publish(st)
	if st.Latest == "" || !Newer(st.Latest, r.Current) {
		return ""
	}
	// This release failed to start here before. A newer one installs as usual.
	if slices.Contains(st.Skip, st.Latest) {
		if r.skipLogged != st.Latest {
			r.skipLogged = st.Latest
			r.logf("update: not installing %s: it failed to start on this node and was rolled back", st.Latest)
		}
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
		r.saveCheck(st)
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
	r.saveCheck(st)
	r.markPending(st.Latest)
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
	err := r.Client.Refusal(tag, sig)
	if r.warned != tag {
		r.warned = tag
		if err != nil {
			r.logf("update: %v", err)
		} else {
			r.logf("update: %s: %s; going ahead, since this client does not require signatures", tag, sigProblem(sig))
		}
	}
	return err == nil
}

// restartWhenIdle waits for the node to serve nothing, until MaxIdleWait or ctx
// ends, then restarts into the installed binary. A failed restart is logged and
// left for the next regular check to try again.
func (r *Runner) restartWhenIdle(ctx context.Context, tag string) {
	r.setPending(true)
	if r.Restart == nil || !r.waitForIdle(ctx) {
		return
	}
	r.markPending(tag)
	if err := r.Restart(r.Executable); err != nil {
		r.logf("update: restart: %v (trying again at the next check, the next start uses %s)", err, tag)
	}
}

// waitForIdle polls until the node serves nothing, or has been busy without a
// break for MaxIdleWait: one stuck client must not hold an update back forever.
// False means ctx ended.
func (r *Runner) waitForIdle(ctx context.Context) bool {
	deadline := time.Now().Add(r.maxIdleWait())
	for ctx.Err() == nil {
		if r.Idle == nil || r.Idle() {
			return true
		}
		if !time.Now().Before(deadline) {
			r.logf("update: the node has been busy for %s, restarting anyway; requests still running are cut", r.maxIdleWait())
			return true
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.poll()):
		}
	}
	return false
}

// MarkHealthy waits until this start has lasted HealthyAfter, then records that
// this version works here, so later restarts are not counted as failed starts.
// main runs it in a goroutine once the gateway is bound.
func (r *Runner) MarkHealthy(ctx context.Context) {
	t := time.NewTimer(r.healthyAfter())
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
		r.markHealthy()
	}
}

func (r *Runner) markHealthy() {
	if !IsRelease(r.Current) {
		return
	}
	r.updateState(func(st *state) {
		st.Healthy = r.Current
		// A pending tag for another version is a release installed and waiting
		// for its restart. Its start has not been counted, and it still must be.
		if st.PendingTag == "" || st.PendingTag == r.Current {
			st.PendingTag, st.From, st.Attempts = "", "", 0
		}
		st.Skip = slices.DeleteFunc(st.Skip, func(tag string) bool { return tag == r.Current })
	})
}

// Stopped is called when a signal has stopped the node. Before it was marked
// healthy that is no failed start, so it stops counting as one: stopping a
// freshly updated node by hand a few times must not look like a bad release.
func (r *Runner) Stopped() {
	r.updateState(func(st *state) {
		if st.PendingTag == r.Current && st.Attempts > 0 {
			st.Attempts--
		}
	})
}

// markPending records that the next start is tag, not yet proven here.
func (r *Runner) markPending(tag string) {
	r.updateState(func(st *state) {
		st.PendingTag, st.From, st.Attempts = tag, r.Current, 0
		if st.Healthy == tag {
			st.Healthy = ""
		}
	})
}

// state is what survives a restart: when we last looked, what we saw, whether
// the file on disk is already that version, and how starts of a new version
// have gone.
type state struct {
	Checked   time.Time `json:"checked,omitempty"`
	Latest    string    `json:"latest,omitempty"`
	Asset     string    `json:"asset,omitempty"`
	Sha256    string    `json:"sha256,omitempty"`
	Signature string    `json:"signature,omitempty"`
	Applied   string    `json:"applied,omitempty"`

	PendingTag     string   `json:"pending_tag,omitempty"`      // a self-update that has not been healthy yet
	From           string   `json:"from,omitempty"`             // the version that installed PendingTag
	Attempts       int      `json:"attempts,omitempty"`         // starts of PendingTag so far
	Healthy        string   `json:"healthy,omitempty"`          // the last version that lasted HealthyAfter
	Skip           []string `json:"skip,omitempty"`             // releases that failed here and are not installed again
	RolledBackFrom string   `json:"rolled_back_from,omitempty"` // read and cleared by the start after a rollback
}

// statePath is the state file. skipPath holds a copy of the skip list, since a
// release older than the list rewrites the state file with only the fields it
// knows. It may run for a long time after a rollback, and a later release still
// has to know what failed.
func statePath(dataDir string) string { return filepath.Join(dataDir, "update.json") }
func skipPath(dataDir string) string  { return filepath.Join(dataDir, "update-skip.json") }

// readState reads the state. A missing file is an empty state. A corrupt one is
// an error, with the state as empty apart from the skip list.
func readState(dataDir string) (state, error) {
	var skip []string
	if b, err := os.ReadFile(skipPath(dataDir)); err == nil && json.Unmarshal(b, &skip) != nil {
		skip = nil
	}
	empty := state{Skip: skip}
	b, err := os.ReadFile(statePath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return empty, fmt.Errorf("%s: %w", statePath(dataDir), err)
	}
	for _, tag := range skip {
		st.Skip = addSkip(st.Skip, tag)
	}
	return st, nil
}

// writeState saves the state and keeps the copy of the skip list in step.
func writeState(dataDir string, st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := writeAtomic(statePath(dataDir), b); err != nil {
		return err
	}
	if len(st.Skip) == 0 {
		if err := os.Remove(skipPath(dataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	skip, err := json.Marshal(st.Skip)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(skipPath(dataDir)); err == nil && bytes.Equal(old, skip) {
		return nil
	}
	return writeAtomic(skipPath(dataDir), skip)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (r *Runner) loadState() state {
	if r.DataDir == "" {
		return state{}
	}
	st, _ := readState(r.DataDir) // a corrupt file is replaced by the next save
	return st
}

func (r *Runner) saveState(st state) {
	if r.DataDir == "" {
		return
	}
	if err := writeState(r.DataDir, st); err != nil {
		r.logf("update: cannot save the update state: %v", err)
	}
}

// updateState changes the state on disk. install holds its copy across a
// download that can take minutes, so it must not write that whole copy back
// over what changed meanwhile, such as the healthy mark.
func (r *Runner) updateState(change func(*state)) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	st := r.loadState()
	change(&st)
	r.saveState(st)
}

// saveCheck saves the fields install owns.
func (r *Runner) saveCheck(check state) {
	r.updateState(func(st *state) {
		st.Checked, st.Latest, st.Asset, st.Sha256, st.Signature, st.Applied =
			check.Checked, check.Latest, check.Asset, check.Sha256, check.Signature, check.Applied
	})
}
