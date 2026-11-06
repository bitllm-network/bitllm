package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// swapIsAtomicHere is whether Swap takes the path that never leaves dst missing.
func swapIsAtomicHere() bool { return runtime.GOOS != "windows" }

// startFixture is a node's files as a self-update leaves them: the new binary,
// the one it replaced, and a data directory.
type startFixture struct {
	s        starter
	data     string
	exe      string
	restarts []string
	lines    []string
}

func newStart(t *testing.T, version string) *startFixture {
	t.Helper()
	dir := t.TempDir()
	f := &startFixture{data: filepath.Join(dir, "data"), exe: filepath.Join(dir, "bitllm")}
	if err := os.Mkdir(f.data, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{f.exe: "new", PrevPath(f.exe): "old"} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.s = starter{
		dataDir: f.data,
		exe:     f.exe,
		version: version,
		restart: func(path string) error { f.restarts = append(f.restarts, path); return nil },
		logf: func(format string, args ...any) {
			t.Logf(format, args...)
			f.lines = append(f.lines, fmt.Sprintf(format, args...))
		},
		versionOf: func(string) (string, error) { return "v0.4.6", nil },
		fs:        osFS,
	}
	return f
}

func (f *startFixture) write(t *testing.T, st state) {
	t.Helper()
	if err := writeState(f.data, st); err != nil {
		t.Fatal(err)
	}
}

func (f *startFixture) read(t *testing.T) state {
	t.Helper()
	st, err := readState(f.data)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *startFixture) logged(sub string) bool {
	return slices.ContainsFunc(f.lines, func(l string) bool { return strings.Contains(l, sub) })
}

// oldState is everything v0.4.6, v0.4.5 and v0.3.1 keep in update.json. They
// read and write it through this struct and drop every other field.
type oldState struct {
	Checked time.Time `json:"checked,omitempty"`
	Latest  string    `json:"latest,omitempty"`
	Asset   string    `json:"asset,omitempty"`
	Sha256  string    `json:"sha256,omitempty"`
	Applied string    `json:"applied,omitempty"`
}

// oldInstalled is the state one of those releases leaves after installing tag.
func oldInstalled(tag string) state {
	return state{Checked: time.Now(), Latest: tag, Asset: "bitllm-linux-amd64", Sha256: strings.Repeat("a", 64), Applied: tag}
}

// oldBinaryChecks does to the state file what one of those releases does when
// its check sees latest, and reports whether it would then install it. The
// condition is the one in their Runner.once.
func oldBinaryChecks(t *testing.T, data, current, latest string) bool {
	t.Helper()
	b, err := os.ReadFile(statePath(data))
	if err != nil {
		t.Fatal(err)
	}
	var st oldState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	st.Checked, st.Latest = time.Now(), latest
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(data), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return !(st.Latest == "" || !Newer(st.Latest, current) || st.Applied == st.Latest)
}

// The hop from a release with no idea of any of this: it only sets Applied and
// leaves a .prev. Three starts fail, and the fourth goes back and restarts.
func TestStartThreeFailedStartsRollBack(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.write(t, oldInstalled(tag))

	for want := 1; want <= maxStartAttempts; want++ {
		if got := f.s.check(); got != "" {
			t.Fatalf("start %d returned %q", want, got)
		}
		if got := f.read(t).Attempts; got != want {
			t.Errorf("after start %d attempts = %d", want, got)
		}
		wantFile(t, f.exe, "new")
		if len(f.restarts) != 0 {
			t.Fatalf("restarted during start %d", want)
		}
	}

	if got := f.s.check(); got != tag {
		t.Errorf("fourth start returned %q, want %q", got, tag)
	}
	wantFile(t, f.exe, "old")
	wantFile(t, FailedPath(f.exe), "new")
	wantNoFile(t, PrevPath(f.exe))
	if !slices.Equal(f.restarts, []string{f.exe}) {
		t.Errorf("restarts = %v, want one into %s", f.restarts, f.exe)
	}
	st := f.read(t)
	if !slices.Equal(st.Skip, []string{tag}) {
		t.Errorf("skip = %v", st.Skip)
	}
	if st.RolledBackFrom != tag || st.Attempts != 0 || st.PendingTag != "" {
		t.Errorf("state = %+v", st)
	}
	if !f.logged("failed to start 3 times") {
		t.Errorf("no clear log line: %v", f.lines)
	}
}

// A restart the node initiated says so itself, with or without Applied.
func TestStartCountsAPendingTag(t *testing.T) {
	const tag = "v0.5.1"
	f := newStart(t, tag)
	f.write(t, state{PendingTag: tag, From: "v0.5.0"})
	for range maxStartAttempts {
		f.s.check()
	}
	if len(f.restarts) != 0 {
		t.Fatal("rolled back too early")
	}
	if got := f.s.check(); got != tag {
		t.Errorf("returned %q", got)
	}
	wantFile(t, f.exe, "old")
	if len(f.restarts) != 1 {
		t.Errorf("restarted %d times", len(f.restarts))
	}
}

// The old binary after a rollback: it has no skip list and rewrites the state
// file without one, but must stay off the failed tag while taking a newer one.
func TestRollbackKeepsAnOldBinaryOffTheFailedTag(t *testing.T) {
	const tag = "v0.5.0"
	for _, tc := range []struct {
		name string
		st   state
	}{
		{"installed by the old binary", oldInstalled(tag)},
		{"state with only a pending tag", state{PendingTag: tag, Attempts: maxStartAttempts}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStart(t, tag)
			tc.st.Attempts = maxStartAttempts
			f.write(t, tc.st)
			if got := f.s.check(); got != tag {
				t.Fatalf("returned %q", got)
			}
			// Six hours apart, as often as it likes.
			for range 3 {
				if oldBinaryChecks(t, f.data, "v0.4.6", tag) {
					t.Fatalf("the old binary would install %s again", tag)
				}
			}
			if !oldBinaryChecks(t, f.data, "v0.4.6", "v0.5.1") {
				t.Error("the old binary would not install a newer release")
			}
		})
	}
}

// The skip list outlives an old binary's rewrite of the state file, so a later
// release still knows.
func TestSkipSurvivesAnOldBinaryRewritingTheState(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	st := oldInstalled(tag)
	st.Attempts = maxStartAttempts
	f.write(t, st)
	f.s.check()

	oldBinaryChecks(t, f.data, "v0.4.6", tag)
	b, err := os.ReadFile(statePath(f.data))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"skip"`) {
		t.Fatalf("the rewrite was expected to drop skip: %s", b)
	}

	if got := f.read(t).Skip; !slices.Equal(got, []string{tag}) {
		t.Errorf("skip after the rewrite = %v", got)
	}
}

func TestStartRollsBackOnlyOnce(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	st := oldInstalled(tag)
	st.Attempts = maxStartAttempts
	f.write(t, st)
	f.s.check()

	// The same process carrying on, or something starting the failed version by
	// hand: there is no .prev left and the count is gone.
	f.restarts = nil
	f.s.check()
	if len(f.restarts) != 0 {
		t.Error("rolled back again")
	}
	wantFile(t, f.exe, "old")
}

// The start after a rollback reports it, and the one after that does not.
func TestStartReportsARollbackOnce(t *testing.T) {
	const failed, target = "v0.5.1", "v0.5.0"
	f := newStart(t, failed)
	f.write(t, state{PendingTag: failed, Attempts: maxStartAttempts})
	f.s.check()

	next := f.s
	next.version = target
	if got := next.check(); got != failed {
		t.Errorf("the start after the rollback returned %q, want %q", got, failed)
	}
	if got := next.check(); got != "" {
		t.Errorf("the start after that returned %q", got)
	}
	// Going back must not count the older release as a new failed update.
	if got := f.read(t); got.Attempts != 0 || got.PendingTag != "" {
		t.Errorf("state = %+v", got)
	}
}

// Where Restart is unavailable (Windows) the swap still happens, and this start
// carries on with the next start using the older binary.
func TestRollbackWithoutRestart(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.s.restart = nil
	f.write(t, state{PendingTag: tag, Attempts: maxStartAttempts})

	if got := f.s.check(); got != tag {
		t.Errorf("returned %q, want %q", got, tag)
	}
	wantFile(t, f.exe, "old")
	if !f.logged("start the node again") {
		t.Errorf("no hint to start again: %v", f.lines)
	}
	if st := f.read(t); st.RolledBackFrom != tag || !slices.Contains(st.Skip, tag) {
		t.Errorf("state = %+v", st)
	}
}

// A failed exec is not fatal either.
func TestRollbackWhenRestartFails(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.s.restart = func(string) error { return errInjected }
	f.write(t, state{PendingTag: tag, Attempts: maxStartAttempts})

	if got := f.s.check(); got != tag {
		t.Errorf("returned %q, want %q", got, tag)
	}
	wantFile(t, f.exe, "old")
	if !f.logged("injected failure") {
		t.Errorf("the failure was not logged: %v", f.lines)
	}
}

func TestStartMissingPrevCarriesOn(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	if err := os.Remove(PrevPath(f.exe)); err != nil {
		t.Fatal(err)
	}
	f.write(t, state{PendingTag: tag, Attempts: maxStartAttempts})

	if got := f.s.check(); got != "" {
		t.Errorf("returned %q", got)
	}
	wantFile(t, f.exe, "new")
	if len(f.restarts) != 0 {
		t.Error("restarted with nothing to go back to")
	}
	if !f.logged("no ") {
		t.Errorf("not logged: %v", f.lines)
	}
	if got := f.read(t).Skip; len(got) != 0 {
		t.Errorf("skip = %v, though nothing was rolled back", got)
	}
}

// A .prev that does not run, or is the same release, is no way back. Swapping
// it in would turn a node that may yet start into one that cannot.
func TestStartUnusablePrevCarriesOn(t *testing.T) {
	const tag = "v0.5.0"
	for name, versionOf := range map[string]func(string) (string, error){
		"does not run": func(string) (string, error) { return "", errInjected },
		"same release": func(string) (string, error) { return tag, nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newStart(t, tag)
			f.s.versionOf = versionOf
			f.write(t, state{PendingTag: tag, Attempts: maxStartAttempts})
			if got := f.s.check(); got != "" {
				t.Errorf("returned %q", got)
			}
			wantFile(t, f.exe, "new")
			wantFile(t, PrevPath(f.exe), "old")
			if len(f.restarts) != 0 {
				t.Error("restarted")
			}
			if got := f.read(t).Skip; len(got) != 0 {
				t.Errorf("skip = %v, though nothing was rolled back", got)
			}
		})
	}
}

func TestStartSwapFailureCarriesOn(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.s.fs = watchedFS(t, f.exe, "rename")
	f.write(t, state{PendingTag: tag, Attempts: maxStartAttempts})

	if got := f.s.check(); got != "" {
		t.Errorf("returned %q", got)
	}
	wantFile(t, f.exe, "new")
	wantFile(t, PrevPath(f.exe), "old")
	if len(f.restarts) != 0 {
		t.Error("restarted")
	}
	// The next start tries again.
	if got := f.read(t).Attempts; got != maxStartAttempts {
		t.Errorf("attempts = %d", got)
	}
}

func TestStartCorruptStateCarriesOn(t *testing.T) {
	const tag = "v0.5.0"
	for name, body := range map[string]string{
		"not json":   "{not json",
		"truncated":  `{"applied":"v0.5.0","attem`,
		"wrong type": `{"applied":"v0.5.0","pending_tag":"v0.5.0","attempts":"three"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newStart(t, tag)
			if err := os.WriteFile(statePath(f.data), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := f.s.check(); got != "" {
				t.Errorf("returned %q", got)
			}
			wantFile(t, f.exe, "new")
			wantFile(t, statePath(f.data), body)
			if len(f.restarts) != 0 {
				t.Error("restarted")
			}
			if !f.logged("cannot read the update state") {
				t.Errorf("not logged: %v", f.lines)
			}
		})
	}
}

// The state path being unreadable is the same case.
func TestStartUnreadableStateCarriesOn(t *testing.T) {
	f := newStart(t, "v0.5.0")
	if err := os.Mkdir(statePath(f.data), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := f.s.check(); got != "" {
		t.Errorf("returned %q", got)
	}
	wantFile(t, f.exe, "new")
}

// A state that cannot be saved is logged and does not stop the start.
func TestStartUnwritableStateCarriesOn(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.s.dataDir = filepath.Join(f.data, "missing", "dir")
	if err := os.MkdirAll(f.s.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.data = f.s.dataDir
	f.write(t, oldInstalled(tag))
	if err := os.Chmod(f.data, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.data, 0o700) })
	if err := os.WriteFile(filepath.Join(f.data, "probe"), nil, 0o600); err == nil {
		t.Skip("this user can write to a read-only directory")
	}
	if got := f.s.check(); got != "" {
		t.Errorf("returned %q", got)
	}
	if !f.logged("cannot save") {
		t.Errorf("not logged: %v", f.lines)
	}
}

// Not every start is a self-update: nothing is counted or written for these.
func TestStartLeavesOtherStartsAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		st      *state
	}{
		{"no state yet", "v0.5.0", nil},
		{"never updated itself", "v0.5.0", &state{Latest: "v0.5.0"}},
		{"installed by hand or by bitllm update", "v0.5.0", &state{Applied: "v0.4.6", Latest: "v0.5.0"}},
		{"already healthy", "v0.5.0", &state{Applied: "v0.5.0", Healthy: "v0.5.0"}},
		{"a build from source", "dev", &state{Applied: "dev", PendingTag: "dev", Attempts: maxStartAttempts}},
		{"a CI build", "ci-abc1234", &state{Applied: "ci-abc1234", Attempts: maxStartAttempts}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStart(t, tc.version)
			var before []byte
			if tc.st != nil {
				f.write(t, *tc.st)
				before, _ = os.ReadFile(statePath(f.data))
			}
			for range maxStartAttempts + 2 {
				if got := f.s.check(); got != "" {
					t.Errorf("returned %q", got)
				}
			}
			after, err := os.ReadFile(statePath(f.data))
			if tc.st == nil {
				if !os.IsNotExist(err) {
					t.Errorf("a state file appeared: %s", after)
				}
			} else if string(after) != string(before) {
				t.Errorf("state changed from %s to %s", before, after)
			}
			wantFile(t, f.exe, "new")
			if len(f.restarts) != 0 {
				t.Error("restarted")
			}
		})
	}
}

func TestStartNeedsADataDirAndABinary(t *testing.T) {
	f := newStart(t, "v0.5.0")
	f.write(t, state{PendingTag: "v0.5.0", Attempts: maxStartAttempts})
	noData, noExe := f.s, f.s
	noData.dataDir = ""
	noExe.exe = ""
	noData.check()
	noExe.check()
	if len(f.restarts) != 0 {
		t.Error("restarted")
	}
	wantFile(t, f.exe, "new")
}

// A healthy start clears the count, and later restarts do not count at all.
func TestHealthyClearsTheCount(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.write(t, oldInstalled(tag))
	f.s.check()
	f.s.check()

	run := &Runner{Current: tag, Executable: f.exe, DataDir: f.data, HealthyAfter: time.Millisecond, Logf: t.Logf}
	run.MarkHealthy(context.Background())

	st := f.read(t)
	if st.Healthy != tag || st.Attempts != 0 || st.PendingTag != "" || st.From != "" {
		t.Fatalf("state = %+v", st)
	}
	before, _ := os.ReadFile(statePath(f.data))
	for range maxStartAttempts + 2 {
		f.s.check()
	}
	after, _ := os.ReadFile(statePath(f.data))
	if string(after) != string(before) {
		t.Errorf("later restarts changed the state from %s to %s", before, after)
	}
	wantFile(t, f.exe, "new")
	if len(f.restarts) != 0 {
		t.Error("restarted")
	}
}

// A start that ends early is not healthy, however it ended.
func TestMarkHealthyStopsWithTheNode(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.write(t, state{PendingTag: tag, Attempts: 1})
	run := &Runner{Current: tag, Executable: f.exe, DataDir: f.data, HealthyAfter: time.Hour, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run.MarkHealthy(ctx)
	if st := f.read(t); st.Healthy != "" || st.Attempts != 1 {
		t.Errorf("state = %+v", st)
	}
}

func TestHealthyAfterDefaultsToAMinute(t *testing.T) {
	if got := (&Runner{}).healthyAfter(); got != 60*time.Second {
		t.Errorf("healthyAfter = %v", got)
	}
}

// A newer release installed and waiting for its restart still has to prove
// itself, so the running version's mark must not clear it.
func TestHealthyKeepsAReleaseWaitingForItsRestart(t *testing.T) {
	f := newStart(t, "v0.5.0")
	f.write(t, state{Applied: "v0.5.1", PendingTag: "v0.5.1", From: "v0.5.0"})
	run := &Runner{Current: "v0.5.0", Executable: f.exe, DataDir: f.data, Logf: t.Logf}
	run.markHealthy()
	st := f.read(t)
	if st.Healthy != "v0.5.0" || st.PendingTag != "v0.5.1" || st.From != "v0.5.0" {
		t.Errorf("state = %+v", st)
	}
	next := f.s
	next.version = "v0.5.1"
	next.check()
	if got := f.read(t).Attempts; got != 1 {
		t.Errorf("the new release's first start counted %d, want 1", got)
	}
}

// A release that fails and later works here is not skipped any more.
func TestHealthyTakesAReleaseOffTheSkipList(t *testing.T) {
	f := newStart(t, "v0.5.0")
	f.write(t, state{Skip: []string{"v0.4.9", "v0.5.0"}})
	run := &Runner{Current: "v0.5.0", Executable: f.exe, DataDir: f.data, Logf: t.Logf}
	run.markHealthy()
	if got := f.read(t).Skip; !slices.Equal(got, []string{"v0.4.9"}) {
		t.Errorf("skip = %v", got)
	}
	run = &Runner{Current: "v0.4.9", Executable: f.exe, DataDir: f.data, Logf: t.Logf}
	run.markHealthy()
	if got := f.read(t).Skip; len(got) != 0 {
		t.Errorf("skip = %v", got)
	}
	wantNoFile(t, skipPath(f.data))
}

// Stopping an updated node by hand within its first minute is no failed start.
func TestStoppedDoesNotCountAsAFailedStart(t *testing.T) {
	const tag = "v0.5.0"
	f := newStart(t, tag)
	f.write(t, oldInstalled(tag))
	run := &Runner{Current: tag, Executable: f.exe, DataDir: f.data, Logf: t.Logf}
	for range maxStartAttempts + 3 {
		f.s.check()
		run.Stopped()
	}
	if len(f.restarts) != 0 {
		t.Error("rolled back after clean stops")
	}
	wantFile(t, f.exe, "new")
	if got := f.read(t).Attempts; got != 0 {
		t.Errorf("attempts = %d", got)
	}
}

func TestStoppedLeavesOtherStatesAlone(t *testing.T) {
	f := newStart(t, "v0.5.0")
	f.write(t, state{PendingTag: "v0.5.1", Attempts: 2})
	(&Runner{Current: "v0.5.0", DataDir: f.data, Logf: t.Logf}).Stopped()
	if got := f.read(t).Attempts; got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// The Runner records the release before restarting into it.
func TestRunnerRecordsThePendingTagBeforeRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	st := state{Attempts: 2, Healthy: "v9.9.9"} // left over from an earlier life
	run.saveState(st)
	restarts := 0
	run.Restart = func(string) error {
		restarts++
		got := run.loadState()
		if got.PendingTag != "v9.9.9" || got.From != "v0.3.0" || got.Attempts != 0 || got.Applied != "v9.9.9" || got.Healthy != "" {
			t.Errorf("state at restart = %+v", got)
		}
		return nil
	}
	run.once(context.Background())
	if restarts != 1 {
		t.Errorf("restarted %d times", restarts)
	}
}

// Where nothing restarts in place, the next start is the one to count.
func TestRunnerRecordsThePendingTagWithoutRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.once(context.Background())
	if got := run.loadState(); got.PendingTag != "v9.9.9" || got.Attempts != 0 {
		t.Errorf("state = %+v", got)
	}
}

// From the Runner installing a release to the fourth start going back, with the
// real swap on both ends. Only the restart is a seam.
func TestUpdateThenFailedStartsGoBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	const tag = "v9.9.9"
	r := newRelease(t, tag, script(tag))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	old := script("v0.3.0")
	os.WriteFile(target, old, 0o755)
	run := runnerFor(t, r, dir, target)
	run.Restart = func(string) error { return nil }
	run.once(context.Background())
	wantFile(t, target, string(r.bin))

	restarts := 0
	s := starter{
		dataDir:   dir,
		exe:       target,
		version:   tag,
		restart:   func(string) error { restarts++; return nil },
		logf:      t.Logf,
		versionOf: reportedVersion,
		fs:        osFS,
	}
	for range maxStartAttempts {
		s.check()
	}
	wantFile(t, target, string(r.bin))
	if got := s.check(); got != tag {
		t.Errorf("returned %q", got)
	}
	wantFile(t, target, string(old))
	wantFile(t, FailedPath(target), string(r.bin))
	if restarts != 1 {
		t.Errorf("restarted %d times", restarts)
	}

	// The old binary, on a build with this code: the same tag again is refused,
	// and the next one is not.
	back := runnerFor(t, r, dir, target)
	back.Restart = func(string) error { t.Error("restarted into the skipped release"); return nil }
	back.CacheFor = time.Nanosecond
	back.once(context.Background())
	wantFile(t, target, string(old))
	if st := back.Status(); st["applied"] != nil || st["pending_restart"] != nil {
		t.Errorf("status = %v, want nothing applied", st)
	}
}

func TestRunnerSkipsATagOnTheList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	for name, st := range map[string]state{
		"never installed here":  {Skip: []string{"v9.9.9"}},
		"named by Applied too":  {Skip: []string{"v9.9.9"}, Applied: "v9.9.9"},
		"in the cached check":   {Skip: []string{"v9.9.9"}, Latest: "v9.9.9", Signature: SigOK, Checked: time.Now()},
		"cached, signed, whole": {Skip: []string{"v9.9.9"}, Latest: "v9.9.9", Signature: SigOK, Checked: time.Now(), Applied: "v9.9.9", Asset: AssetName(), Sha256: strings.Repeat("a", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, "v9.9.9", script("v9.9.9"))
			dir := t.TempDir()
			target := filepath.Join(dir, "bitllm")
			os.WriteFile(target, []byte("old"), 0o755)
			run := runnerFor(t, r, dir, target)
			run.saveState(st)
			run.Restart = func(string) error {
				t.Error("restarted into a skipped release")
				return nil
			}
			logs := captureLogs(run)

			run.once(context.Background())
			run.once(context.Background())

			wantFile(t, target, "old")
			if r.assetHits.Load() != 0 {
				t.Errorf("downloaded it %d times", r.assetHits.Load())
			}
			if got := run.Status(); got["pending_restart"] != nil || got["applied"] != nil {
				t.Errorf("status = %v", got)
			}
			if n := logs.count("failed to start on this node"); n != 1 {
				t.Errorf("logged the skip %d times, want once: %v", n, logs.all())
			}
			if n := logs.count("is available"); n != 0 {
				t.Errorf("called a skipped release available: %v", logs.all())
			}
		})
	}
}

// A newer tag clears nothing and installs normally.
func TestRunnerInstallsATagNewerThanTheSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.saveState(state{Skip: []string{"v9.9.8"}, Applied: "v9.9.8"})
	restarts := 0
	run.Restart = func(string) error { restarts++; return nil }

	run.once(context.Background())

	wantFile(t, target, string(r.bin))
	if restarts != 1 {
		t.Errorf("restarted %d times", restarts)
	}
	st := run.loadState()
	if !slices.Equal(st.Skip, []string{"v9.9.8"}) || st.Applied != "v9.9.9" {
		t.Errorf("state = %+v", st)
	}
}

// hookTransport runs a function as each request goes out.
type hookTransport struct {
	base   http.RoundTripper
	before func(*http.Request)
}

func (h hookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.before(req)
	return h.base.RoundTrip(req)
}

// A check keeps its copy of the state across the network. The healthy mark made
// meanwhile must not be overwritten by it, or a healthy node's next restart
// would count as a failed start.
func TestACheckInProgressKeepsTheHealthyMark(t *testing.T) {
	r := newRelease(t, "v0.3.0", script("v0.3.0"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.saveState(state{PendingTag: "v0.3.0", Attempts: 1})
	var once atomic.Bool
	run.Client.HTTP = &http.Client{Transport: hookTransport{
		base: r.server.Client().Transport,
		before: func(*http.Request) {
			if once.CompareAndSwap(false, true) {
				run.markHealthy()
			}
		},
	}}

	run.once(context.Background())

	st := run.loadState()
	if st.Healthy != "v0.3.0" || st.Attempts != 0 || st.PendingTag != "" {
		t.Errorf("state = %+v", st)
	}
	if st.Latest != "v0.3.0" {
		t.Errorf("the check itself was not saved: %+v", st)
	}
}

func TestStatusShowsARollback(t *testing.T) {
	run := &Runner{Current: "v0.4.9"}
	if got := run.Status()["rolled_back_from"]; got != nil {
		t.Errorf("rolled_back_from = %v on a normal start", got)
	}
	run.RolledBackFrom = "v0.5.0"
	if got := run.Status()["rolled_back_from"]; got != "v0.5.0" {
		t.Errorf("rolled_back_from = %v", got)
	}
}

func TestSkipListIsBounded(t *testing.T) {
	var skip []string
	for i := range maxSkip + 5 {
		skip = addSkip(skip, fmt.Sprintf("v0.0.%d", i))
		skip = addSkip(skip, fmt.Sprintf("v0.0.%d", i))
	}
	if len(skip) != maxSkip {
		t.Errorf("%d entries, want %d", len(skip), maxSkip)
	}
	if want := fmt.Sprintf("v0.0.%d", maxSkip+4); skip[len(skip)-1] != want {
		t.Errorf("newest entry = %q, want %q", skip[len(skip)-1], want)
	}
}

// A corrupt copy of the skip list is the same as none.
func TestCorruptSkipFileIsIgnored(t *testing.T) {
	f := newStart(t, "v0.5.0")
	f.write(t, state{Skip: []string{"v0.4.9"}})
	if err := os.WriteFile(skipPath(f.data), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t).Skip; !slices.Equal(got, []string{"v0.4.9"}) {
		t.Errorf("skip = %v", got)
	}
}

// The copy of the skip list survives a corrupt state file too, and a Runner
// reads what is left and replaces the file at its next save.
func TestRunnerCarriesOnFromACorruptState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	r := newRelease(t, "v9.9.9", script("v9.9.9"))
	dir := t.TempDir()
	target := filepath.Join(dir, "bitllm")
	os.WriteFile(target, []byte("old"), 0o755)
	run := runnerFor(t, r, dir, target)
	run.saveState(state{Skip: []string{"v9.9.9"}})
	if err := os.WriteFile(statePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	run.Restart = func(string) error { t.Error("restarted"); return nil }

	run.once(context.Background())

	wantFile(t, target, "old")
	if _, err := readState(dir); err != nil {
		t.Errorf("the state is still corrupt: %v", err)
	}
}

// reportedVersion is what rollBack trusts about a .prev.
func TestReportedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if got, err := reportedVersion(write("good", "#!/bin/sh\necho bitllm v0.4.6\n")); err != nil || got != "v0.4.6" {
		t.Errorf("good binary = %q, %v", got, err)
	}
	if _, err := reportedVersion(write("other", "#!/bin/sh\necho hello\n")); err == nil {
		t.Error("accepted a binary that is not bitllm")
	}
	if _, err := reportedVersion(write("fails", "#!/bin/sh\nexit 3\n")); err == nil {
		t.Error("accepted a binary that exits non-zero")
	}
	if _, err := reportedVersion(write("junk", "not a program")); err == nil {
		t.Error("accepted a file that does not run")
	}
	if _, err := reportedVersion(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file = %v", err)
	}
}
