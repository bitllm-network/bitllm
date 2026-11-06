package update

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// maxStartAttempts is how many starts a release that has not yet been healthy
// gets. The start after them rolls back.
const maxStartAttempts = 3

// maxSkip bounds the skip list. Each entry takes three failed starts of a new
// release, so this keeps far more than a node will meet.
const maxSkip = 32

// StartCheck counts this start of a self-updated release and, once the release
// has failed too often, goes back to the binary it replaced. It returns the
// release a rollback just left, if the last start rolled back, for Status.
//
// Call it at the start of the node command only, with the parsed --data, and
// never for --version or any other subcommand: Fetch and install run a new
// binary with --version, and that must not touch state or swap anything. It
// lives entirely in the new binary, so it also covers the hop from a release
// that knows nothing about it.
//
// restart replaces this process with a binary, as Runner.Restart does; nil
// where the platform cannot. Nothing here is fatal: any error is logged and the
// node carries on starting.
//
// What this covers:
//   - A supervisor that starts the node again (systemd with Restart=always,
//     launchd with KeepAlive). Each failed start exits, the next one counts, and
//     the fourth goes back by itself.
//   - A plain terminal with nothing restarting it. A failed start just exits, and
//     the count moves only when a person runs the node again, so the fourth run
//     by hand is the one that goes back.
//
// What it cannot cover: a crash before main reaches this call, such as a broken
// init or a flag the new binary no longer parses, since none of this code runs;
// and a start that hangs without exiting, until something restarts it.
func StartCheck(dataDir, exe, version string, restart func(path string) error) string {
	return starter{
		dataDir:   dataDir,
		exe:       exe,
		version:   version,
		restart:   restart,
		logf:      log.Printf,
		versionOf: reportedVersion,
		fs:        osFS,
	}.check()
}

// starter is one StartCheck, with what a test replaces.
type starter struct {
	dataDir, exe, version string
	restart               func(path string) error
	logf                  func(format string, args ...any)
	versionOf             func(path string) (string, error) // what a binary reports for --version
	fs                    fsOps
}

func (s starter) check() string {
	// A build from source or from CI must never replace itself.
	if !IsRelease(s.version) || s.dataDir == "" || s.exe == "" {
		return ""
	}
	st, err := readState(s.dataDir)
	if err != nil {
		s.logf("update: cannot read the update state, not counting this start: %v", err)
		return ""
	}
	// A rollback is reported by the start after it, and not by later ones.
	rolledBackFrom := st.RolledBackFrom
	st.RolledBackFrom = ""
	changed := rolledBackFrom != ""

	if s.unconfirmed(st) {
		if st.Attempts >= maxStartAttempts {
			if failed := s.rollBack(st); failed != "" {
				return failed
			}
		} else {
			st.Attempts++
			st.PendingTag = s.version
			changed = true
			s.logf("update: %s has not been healthy yet, this is start %d of %d before going back", s.version, st.Attempts, maxStartAttempts)
		}
	}
	if changed {
		s.save(st)
	}
	return rolledBackFrom
}

// unconfirmed reports whether this version is a self-update that has not yet
// been healthy. Either a Runner said so before restarting into it, or it is the
// hop from a release that only sets Applied and leaves a .prev.
func (s starter) unconfirmed(st state) bool {
	if st.Healthy == s.version {
		return false
	}
	if st.PendingTag == s.version {
		return true
	}
	if st.Applied != s.version {
		return false
	}
	_, err := os.Stat(PrevPath(s.exe))
	return err == nil
}

// rollBack puts the previous binary back and restarts into it. It returns the
// release it left, or "" if the node has to carry on with this binary.
func (s starter) rollBack(st state) string {
	failed := s.version
	prev := PrevPath(s.exe)
	if _, err := os.Stat(prev); err != nil {
		s.logf("update: %s failed to start %d times and there is no %s to go back to, carrying on", failed, st.Attempts, prev)
		return ""
	}
	// A .prev that does not run, or is this same release, would leave the node
	// no better off with no way forward.
	target, err := s.versionOf(prev)
	if err != nil {
		s.logf("update: %s failed to start %d times but %s does not run (%v), carrying on", failed, st.Attempts, prev, err)
		return ""
	}
	if target == failed {
		s.logf("update: %s failed to start %d times but %s is the same release, carrying on", failed, st.Attempts, prev)
		return ""
	}

	// The skip list goes first. A crash after this repeats the rollback, since
	// the count is still there; a crash after the swap still knows to skip.
	st.Skip = addSkip(st.Skip, failed)
	s.save(st)
	if err := swapBack(s.fs, s.exe); err != nil {
		s.logf("update: %s failed to start %d times but going back to %s failed, carrying on: %v", failed, st.Attempts, target, err)
		return ""
	}

	// Applied keeps naming the failed release, though it is no longer on disk.
	// A release older than this code has no skip list and rewrites this file
	// without one, but it never installs the tag Applied names, so this is what
	// keeps such a .prev from fetching the failed release again.
	st.Applied = failed
	st.PendingTag, st.From, st.Attempts = "", "", 0
	st.RolledBackFrom = failed
	s.save(st)
	s.logf("update: %s failed to start %d times, went back to %s; %s will not be installed again, a newer release will", failed, maxStartAttempts, target, failed)

	if s.restart == nil {
		s.logf("update: start the node again to use %s", target)
		return failed
	}
	s.logf("update: restarting into %s", target)
	if err := s.restart(s.exe); err != nil {
		s.logf("update: restart: %v (the next start uses %s)", err, target)
	}
	return failed
}

func (s starter) save(st state) {
	if err := writeState(s.dataDir, st); err != nil {
		s.logf("update: cannot save the update state: %v", err)
	}
}

// addSkip adds tag to the list once, and drops the oldest past maxSkip.
func addSkip(skip []string, tag string) []string {
	if slices.Contains(skip, tag) {
		return skip
	}
	skip = append(skip, tag)
	if len(skip) > maxSkip {
		skip = skip[len(skip)-maxSkip:]
	}
	return skip
}

// reportedVersion runs path --version and returns the version it names.
func reportedVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", err
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "bitllm ")
	if !ok {
		return "", fmt.Errorf("it reports %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}
