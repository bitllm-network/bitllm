package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bitllm-org/bitllm/internal/node"
	"github.com/bitllm-org/bitllm/internal/update"
)

// executable is the real path of the running binary, following the symlink a
// package manager or ~/.local/bin may have put in front of it.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	return p, nil
}

// updateCmd is `bitllm update [--check] [--tag vX.Y.Z]`: the same check the
// node makes in the background, on demand, without a restart in place. Useful
// in a supervisor setup, and the whole story on Windows.
func updateCmd(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "print the release and its signature result and exit without installing it")
	tag := fs.String("tag", "", "use this release instead of the newest one, e.g. a prerelease or an older version (vX.Y.Z)")
	fs.Parse(args)

	exe, err := executable()
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	opts := updateOptions{check: *check, tag: *tag, exe: exe, current: node.Version}
	if err := runUpdate(ctx, update.New(), opts, os.Stdout); err != nil {
		log.Fatalf("update: %v", err)
	}
}

type updateOptions struct {
	check   bool
	tag     string // empty: the newest release
	exe     string // the binary to replace
	current string // the version it is running
}

// runUpdate finds the release, prints what it found, and installs it unless
// o.check is set or there is nothing to do.
func runUpdate(ctx context.Context, c *update.Client, o updateOptions, w io.Writer) error {
	var rel update.Release
	var err error
	if o.tag != "" {
		rel, err = c.Tag(ctx, o.tag)
	} else {
		rel, err = c.Latest(ctx)
	}
	if err != nil {
		return err
	}
	if o.tag != "" {
		fmt.Fprintf(w, "release: %s\n", rel.Tag)
	} else {
		fmt.Fprintf(w, "newest release: %s\n", rel.Tag)
	}
	fmt.Fprintf(w, "signature: %s\n", rel.Signature)
	if o.tag != "" && rel.Tag == o.current {
		fmt.Fprintf(w, "bitllm %s is already this release\n", o.current)
		return nil
	}
	if o.tag == "" && update.IsRelease(o.current) && !update.Newer(rel.Tag, o.current) {
		fmt.Fprintf(w, "bitllm %s is up to date\n", o.current)
		return nil
	}
	fmt.Fprintf(w, "this binary: %s (%s)\n", o.current, o.exe)
	if o.check {
		return nil
	}
	path, err := c.Fetch(ctx, rel, filepath.Dir(o.exe))
	if err != nil {
		return err
	}
	if err := update.Swap(path, o.exe); err != nil {
		os.Remove(path)
		return fmt.Errorf("cannot replace %s: %w", o.exe, err)
	}
	fmt.Fprintf(w, "installed %s, previous binary kept as %s\n", rel.Tag, update.PrevPath(o.exe))
	fmt.Fprintln(w, "restart the node to use it")
	if o.tag != "" {
		fmt.Fprintf(w, "a running node with auto-update moves forward to the newest release again; start it with --auto-update=false to stay on %s\n", rel.Tag)
	}
	return nil
}
