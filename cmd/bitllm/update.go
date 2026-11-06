package main

import (
	"context"
	"flag"
	"fmt"
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

// updateCmd is `bitllm update [--check]`: the same check the node makes in
// the background, on demand, without a restart in place. Useful in a
// supervisor setup, and the whole story on Windows.
func updateCmd(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "print the newest release and exit without installing it")
	fs.Parse(args)

	exe, err := executable()
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c := update.New()
	rel, err := c.Latest(ctx)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	fmt.Printf("newest release: %s\n", rel.Tag)
	fmt.Printf("signature: %s\n", rel.Signature)
	if update.IsRelease(node.Version) && !update.Newer(rel.Tag, node.Version) {
		fmt.Printf("bitllm %s is up to date\n", node.Version)
		return
	}
	fmt.Printf("this binary: %s (%s)\n", node.Version, exe)
	if *check {
		return
	}
	path, err := c.Fetch(ctx, rel, filepath.Dir(exe))
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	if err := update.Swap(path, exe); err != nil {
		os.Remove(path)
		log.Fatalf("update: cannot replace %s: %v", exe, err)
	}
	fmt.Printf("installed %s, previous binary kept as %s\n", rel.Tag, update.PrevPath(exe))
	fmt.Println("restart the node to use it")
}
