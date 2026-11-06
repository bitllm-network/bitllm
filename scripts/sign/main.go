// Command sign makes and checks BitLLM release signatures. Each admin holds one
// private key, offline; the public halves are embedded in
// internal/update/keys.go. See update.Message for what a signature covers.
//
//	go run ./scripts/sign keygen OUTFILE
//	go run ./scripts/sign sign --key FILE --tag vX.Y.Z --sums PATH
//	go run ./scripts/sign verify --tag vX.Y.Z --sums PATH --sig PATH
//	go run ./scripts/sign check-keys
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bitllm-org/bitllm/internal/update"
)

const usage = `usage:
  sign keygen OUTFILE
  sign sign --key FILE --tag vX.Y.Z --sums PATH
  sign verify --tag vX.Y.Z --sums PATH --sig PATH
  sign check-keys`

var errUsage = errors.New(usage)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process around it. Every failure exits 1.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 1
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stdout)
	case "sign":
		err = sign(args[1:], stdout)
	case "verify":
		err = verify(args[1:], stdout)
	case "check-keys":
		err = checkKeys(args[1:], stdout)
	default:
		err = errUsage
	}
	if err != nil {
		fmt.Fprintf(stderr, "sign %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// keygen writes a new private key to a file only the owner can read, and
// prints the public half. The file holds the base64 of the 32-byte seed.
func keygen(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errUsage
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(encode(priv.Seed()) + "\n")
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(args[0])
		return err
	}
	fmt.Fprintln(stdout, encode(pub))
	return nil
}

// sign prints one signature line, ready to append to SHA256SUMS.sig.
func sign(args []string, stdout io.Writer) error {
	fs := flagSet("sign")
	key := fs.String("key", "", "private key file")
	tag := fs.String("tag", "", "release tag, vX.Y.Z")
	sums := fs.String("sums", "", "path of SHA256SUMS")
	if err := parse(fs, args, key, tag, sums); err != nil {
		return err
	}
	if err := checkTag(*tag); err != nil {
		return err
	}
	priv, err := readKey(*key)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(*sums)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, encode(ed25519.Sign(priv, update.Message(*tag, body))))
	return nil
}

// verify checks a signature file against the keys built into this tree, the
// same check a node makes.
func verify(args []string, stdout io.Writer) error {
	fs := flagSet("verify")
	tag := fs.String("tag", "", "release tag, vX.Y.Z")
	sums := fs.String("sums", "", "path of SHA256SUMS")
	sig := fs.String("sig", "", "path of SHA256SUMS.sig")
	if err := parse(fs, args, tag, sums, sig); err != nil {
		return err
	}
	if err := checkTag(*tag); err != nil {
		return err
	}
	body, err := os.ReadFile(*sums)
	if err != nil {
		return err
	}
	lines, err := os.ReadFile(*sig)
	if err != nil {
		return err
	}
	if err := update.Verify(*tag, body, lines); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: %s is signed by a trusted key\n", *tag)
	return nil
}

// checkKeys fails unless the tree holds at least one key and every entry is a
// valid key, so a tag cannot be cut from a tree no node could check against.
func checkKeys(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errUsage
	}
	if len(update.TrustedKeys) == 0 {
		return errors.New("update.TrustedKeys is empty: add the admins' public keys to internal/update/keys.go before a tag")
	}
	for i, s := range update.TrustedKeys {
		if _, err := update.ParseKey(s); err != nil {
			return fmt.Errorf("update.TrustedKeys[%d] %q is not a valid ed25519 public key: %w", i, s, err)
		}
	}
	fmt.Fprintf(stdout, "ok: %d trusted release key(s)\n", len(update.TrustedKeys))
	return nil
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // run reports the error once, with the usage
	return fs
}

// parse reads the flags and requires every one of them to be set.
func parse(fs *flag.FlagSet, args []string, required ...*string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, usage)
	}
	if fs.NArg() != 0 {
		return errUsage
	}
	for _, v := range required {
		if *v == "" {
			return errUsage
		}
	}
	return nil
}

func checkTag(tag string) error {
	if !update.IsRelease(tag) {
		return fmt.Errorf("%q is not a release tag, want vX.Y.Z", tag)
	}
	return nil
}

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a key file from keygen", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
