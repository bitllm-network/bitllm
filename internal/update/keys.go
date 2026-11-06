package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// TrustedKeys are the base64 ed25519 public keys a release may be signed with,
// one per admin; a signature from any of them is enough. Empty for now: the
// admins' public keys go here before a tag, and `go run ./scripts/sign
// check-keys` fails until they do.
var TrustedKeys = []string{}

// requireSignature is whether a release has to be signed to be installed. This
// release checks and reports only; the next one turns it on. See
// Client.RequireSignature.
const requireSignature = false

// What a signature check can come to, as Release.Signature and GET /status
// report it.
const (
	SigOK      = "ok"
	SigMissing = "missing" // no SHA256SUMS.sig could be read
	SigBad     = "bad"     // no line verifies under any trusted key
	SigNoKeys  = "no-keys" // this build has nothing to check against
)

// Why Verify did not accept a release.
var (
	ErrNoKeys     = errors.New("this build has no trusted release keys")
	ErrSigMissing = errors.New("the release has no signature")
	ErrSigBad     = errors.New("the release signature does not verify")
)

// maxSigSize bounds SHA256SUMS.sig. One line is under 100 bytes, so this holds
// far more signatures than there will ever be admins.
const maxSigSize = 64 << 10

// Message is what a release signature covers: the exact checksum file, bound
// to the tag it was published under.
func Message(tag string, sums []byte) []byte {
	return append([]byte("bitllm release v1\n"+tag+"\n"), sums...)
}

// ParseKey reads one TrustedKeys entry: the standard base64 of a 32-byte
// ed25519 public key.
func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// Verify checks sig, the contents of SHA256SUMS.sig, against sums, the raw
// bytes of SHA256SUMS. The file holds one standard base64 signature per line,
// so more than one admin can sign a release; blank lines do not count, and
// nothing past maxSigSize is read. A release is signed if any line verifies
// under any trusted key. The error is ErrNoKeys, ErrSigMissing or ErrSigBad.
//
// A TrustedKeys entry that does not parse is skipped rather than reported: a
// running node must not fail over it, and check-keys stops such a build from
// being released.
func Verify(tag string, sums, sig []byte) error {
	var keys []ed25519.PublicKey
	for _, s := range TrustedKeys {
		if k, err := ParseKey(s); err == nil {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ErrNoKeys
	}
	if len(sig) > maxSigSize {
		sig = sig[:maxSigSize]
	}
	msg := Message(tag, sums)
	lines := 0
	for line := range bytes.SplitSeq(sig, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		lines++
		raw, err := base64.StdEncoding.Strict().DecodeString(string(line))
		if err != nil || len(raw) != ed25519.SignatureSize {
			continue
		}
		for _, k := range keys {
			if ed25519.Verify(k, msg, raw) {
				return nil
			}
		}
	}
	if lines == 0 {
		return ErrSigMissing
	}
	return ErrSigBad
}

// sigResult turns a Verify error into the value Release.Signature carries.
func sigResult(err error) string {
	switch {
	case err == nil:
		return SigOK
	case errors.Is(err, ErrNoKeys):
		return SigNoKeys
	case errors.Is(err, ErrSigMissing):
		return SigMissing
	}
	return SigBad
}

// sigProblem says in words what is wrong with a Signature value other than
// SigOK. Empty means no check has run, as in a state file from an older build.
func sigProblem(sig string) string {
	switch sig {
	case SigMissing:
		return "it has no signature"
	case SigBad:
		return "its signature does not verify"
	case SigNoKeys:
		return "this build has no release keys to check it with"
	}
	return "its signature has not been checked"
}
