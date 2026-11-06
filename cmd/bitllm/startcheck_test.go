package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/node"
	"github.com/bitllm-org/bitllm/internal/update"
)

// runMainEnv makes this test binary run main itself, as a release of the version
// it names, so these tests see the real order of things in main.
const runMainEnv = "BITLLM_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if v := os.Getenv(runMainEnv); v != "" {
		node.Version = v
		main()
		return
	}
	os.Exit(m.Run())
}

const failingRelease = "v9.9.9"

// prevScript stands in for the previous binary: it reports its version, and
// otherwise prints the arguments it was started with.
const prevScript = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "bitllm v0.4.6"; exit 0; fi
echo "previous binary ran: $*"
`

// install is a copy of this test binary set up as a release that has failed all
// its starts: one more node start would go back to the .prev next to it.
type install struct {
	exe, home, data string
	before          os.FileInfo
	state           []byte
}

func newInstall(t *testing.T, st map[string]any) *install {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the previous binary is a shell script")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := &install{exe: filepath.Join(dir, "bitllm"), home: filepath.Join(dir, "home")}
	in.data = filepath.Join(in.home, ".bitllm")
	if err := os.MkdirAll(in.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(self, in.exe); err != nil {
		if err := copyFile(self, in.exe); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(update.PrevPath(in.exe), []byte(prevScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if in.state, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.statePath(), in.state, 0o600); err != nil {
		t.Fatal(err)
	}
	if in.before, err = os.Stat(in.exe); err != nil {
		t.Fatal(err)
	}
	return in
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (in *install) statePath() string { return filepath.Join(in.data, "update.json") }

func (in *install) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, in.exe, args...)
	cmd.Env = append(os.Environ(), runMainEnv+"="+failingRelease, "HOME="+in.home)
	return cmd
}

// wantUntouched fails if anything a start check owns has moved.
func (in *install) wantUntouched(t *testing.T) {
	t.Helper()
	now, err := os.Stat(in.exe)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(in.before, now) {
		t.Error("the binary was replaced")
	}
	if _, err := os.Stat(update.PrevPath(in.exe)); err != nil {
		t.Errorf("the previous binary is gone: %v", err)
	}
	b, err := os.ReadFile(in.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(in.state) {
		t.Errorf("update.json changed from %s to %s", in.state, b)
	}
}

func failedAllItsStarts() map[string]any {
	return map[string]any{"applied": failingRelease, "latest": failingRelease, "pending_tag": failingRelease, "attempts": 3}
}

// Fetch and install run a new binary with --version, and people run the rest by
// hand on a node that may be in any state. None of them is a start of the node.
func TestOnlyTheNodeCommandTouchesUpdateState(t *testing.T) {
	for _, args := range [][]string{
		{"--version"},
		{"--version", "--data", ""}, // replaced below with the data directory
		{"update", "--no-such-flag"},
		{"account", "--no-such-flag"},
		{"invite", "--no-such-flag"},
		{"top", "--no-such-flag"},
		{"hash-model"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			in := newInstall(t, failedAllItsStarts())
			args := slices.Clone(args)
			if i := slices.Index(args, "--data"); i >= 0 {
				args[i+1] = in.data
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := in.command(ctx, args...).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("still running: %s", out)
			}
			t.Logf("exit %v: %s", err, firstLine(out))
			if strings.Contains(string(out), "previous binary ran") {
				t.Errorf("it restarted into the previous binary: %s", out)
			}
			in.wantUntouched(t)
		})
	}
}

// The other side of that: a real start of the node goes back, through the real
// exec, with its arguments, using the --data it was given.
func TestNodeStartGoesBackThroughMain(t *testing.T) {
	in := newInstall(t, failedAllItsStarts())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// If it does not go back, these flags keep the node off any real network.
	args := []string{"--data", in.data, "--dht-server", "--mdns=false", "--upnp=false", "--port", "0", "--gateway", "127.0.0.1:0"}
	out, err := in.command(ctx, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if want := "previous binary ran: " + strings.Join(args, " "); !strings.Contains(string(out), want) {
		t.Errorf("output is missing %q:\n%s", want, out)
	}
	if b, _ := os.ReadFile(in.exe); string(b) != prevScript {
		t.Error("the previous binary is not back in place")
	}
	if _, err := os.Stat(update.FailedPath(in.exe)); err != nil {
		t.Errorf("the failed binary was not kept: %v", err)
	}
	var st map[string]any
	b, err := os.ReadFile(in.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st["rolled_back_from"] != failingRelease {
		t.Errorf("state = %s", b)
	}
	if skip, _ := st["skip"].([]any); len(skip) != 1 || skip[0] != failingRelease {
		t.Errorf("state = %s", b)
	}
}

// runNodeUntilUp starts the node, waits for it to bind its gateway, and stops it
// with SIGTERM. It only ever talks to itself: no seeds, no mDNS, no UPnP.
func runNodeUntilUp(t *testing.T, in *install, extra ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := append([]string{"--data", in.data, "--dht-server", "--mdns=false", "--upnp=false", "--port", "0", "--gateway", "127.0.0.1:0"}, extra...)
	cmd := in.command(ctx, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewScanner(stderr)
	up := false
	for scan.Scan() {
		if strings.Contains(scan.Text(), "gateway http://") {
			up = true
			break
		}
	}
	if !up {
		t.Fatalf("the node never came up: %v", cmd.Wait())
	}
	go io.Copy(io.Discard, stderr)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("node exit: %v", err)
	}
}

// --auto-update=false promises to stay on the version you have.
func TestPinnedNodeDoesNotGoBack(t *testing.T) {
	in := newInstall(t, failedAllItsStarts())
	runNodeUntilUp(t, in, "--auto-update=false")
	in.wantUntouched(t)
}

// A real start of an unconfirmed release counts, and a clean stop takes it back.
func TestNodeStartCountsAndACleanStopDoesNot(t *testing.T) {
	st := failedAllItsStarts()
	st["attempts"] = 1
	in := newInstall(t, st)

	// While it runs, the count is one higher. A clean stop restores it, so the
	// file ends as it began apart from field order, which is compared parsed.
	runNodeUntilUp(t, in)

	var got map[string]any
	b, err := os.ReadFile(in.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["attempts"] != float64(1) || got["pending_tag"] != failingRelease {
		t.Errorf("state = %s", b)
	}
	now, err := os.Stat(in.exe)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(in.before, now) {
		t.Error("the binary was replaced")
	}
}

// A dev build and a pinned node never call into the rollback at all.
func TestStartCheckLeavesDevBuildsAlone(t *testing.T) {
	if update.IsRelease(node.Version) {
		t.Skip("this test binary was built as a release")
	}
	in := newInstall(t, map[string]any{"applied": node.Version, "pending_tag": node.Version, "attempts": 3})
	if got := startCheck(in.data, true); got != "" {
		t.Errorf("startCheck = %q", got)
	}
	in.wantUntouched(t)
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}
