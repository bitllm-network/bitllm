// Package receipts proves work exchanged with credits. After each job the
// client signs what it received (token counts and a hash of the exact
// response bytes), the server checks that against what it sent and
// countersigns, then gossips the receipt. Any node can verify both
// signatures and tally credits per server, so the record is public and
// checkable without a chain. Which receipts will count (self-dealing,
// unverified models) is decided later, from these facts.
//
// The client signs with a pseudonym, not its node key: one key per server,
// rotated every 30 days, derived from a private seed. So the public record
// shows how many distinct clients each server had in a month (a one-client
// server farming itself stands out) but cannot link a client across servers
// or across months. Distinct keys are free, so this is a weak signal only.
package receipts

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/bitllm-org/bitllm/internal/store"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type Receipt struct {
	Job        string `json:"job"`
	Model      string `json:"model"`
	Client     string `json:"client"`
	Server     string `json:"server"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Credits    int64  `json:"credits"`
	Output     string `json:"output_sha256"`
	TS         int64  `json:"ts"`
}

type SignedReceipt struct {
	Receipt   Receipt `json:"receipt"`
	ClientSig []byte  `json:"client_sig"`
	ServerSig []byte  `json:"server_sig,omitempty"`
}

// Bytes is the exact text both parties sign.
func (r Receipt) Bytes() []byte { b, _ := json.Marshal(r); return b }

// maxJobTokens bounds the token counts of one receipt, so a pair of colluding
// keys cannot mint arbitrary credits in a single signed line.
const maxJobTokens = 4 << 20

// Price is the credits for a job: prefill is cheap relative to decode.
func Price(prompt, completion int64) int64 { return prompt/4 + completion }

// Valid checks both signatures and that the credits follow from the token counts.
func (sr *SignedReceipt) Valid() bool {
	r := sr.Receipt
	if r.Prompt < 0 || r.Completion < 0 || r.Prompt > maxJobTokens || r.Completion > maxJobTokens ||
		r.Credits != Price(r.Prompt, r.Completion) || len(r.Job) > 64 || len(r.Model) > 128 {
		return false
	}
	b := r.Bytes()
	return protocol.VerifySig(r.Client, b, sr.ClientSig) && protocol.VerifySig(r.Server, b, sr.ServerSig)
}

// pendingJob is what a server remembers about a job until the client's receipt arrives.
type pendingJob struct {
	client     peer.ID
	model      string
	prompt     int64
	completion int64
	output     string
	credits    int64
	expires    time.Time
}

type ServerCredits struct {
	Credits int64 `json:"credits"`
	Jobs    int   `json:"jobs"`
}

// Gossip limits. Valid receipts cost nothing to forge between two keys you
// own, so without limits anyone could fill every node's memory and disk.
// A receipt must be published by its own server, be recent, and fit under a
// per-server and a network-wide rate.
const (
	receiptMaxAge    = time.Hour
	receiptPerServer = 120 // per minute
	receiptPerMinute = 600 // per minute, all servers together
)

type gossipRate struct {
	mu     sync.Mutex
	minute int64
	total  int
	per    map[string]int
}

func (g *gossipRate) allow(server string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m := now.Unix() / 60; m != g.minute {
		g.minute, g.total, g.per = m, 0, map[string]int{}
	}
	if g.total >= receiptPerMinute || g.per[server] >= receiptPerServer {
		return false
	}
	g.total++
	g.per[server]++
	return true
}

// AcceptGossip is the topic validator for receipts.
func (rs *Receipts) AcceptGossip(from peer.ID, sr *SignedReceipt) bool {
	r, now := sr.Receipt, time.Now()
	if from.String() != r.Server || r.TS < now.Add(-receiptMaxAge).Unix() || r.TS > now.Add(10*time.Minute).Unix() {
		return false
	}
	return sr.Valid() && rs.rate.allow(r.Server, now)
}

// Served is called by the server when a job finishes.
func (rs *Receipts) Served(job string, client peer.ID, model string, prompt, completion int64, output string, credits int64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	now := time.Now()
	for id, p := range rs.pending {
		if now.After(p.expires) {
			delete(rs.pending, id)
		}
	}
	rs.pending[job] = &pendingJob{client: client, model: model, prompt: prompt, completion: completion,
		output: output, credits: credits, expires: now.Add(10 * time.Minute)}
}

// ErrUnknownJob means the receipt arrived before we recorded the job. Its text
// is what goes over the wire, so the client can tell the two cases apart.
var ErrUnknownJob = errors.New("unknown job")

// Countersign checks a client's receipt against the job we served and signs it.
func (rs *Receipts) Countersign(sr *SignedReceipt, from peer.ID) error {
	r := sr.Receipt
	rs.mu.Lock()
	p := rs.pending[r.Job]
	delete(rs.pending, r.Job)
	rs.mu.Unlock()
	switch {
	case p == nil:
		return ErrUnknownJob
	case r.Server != rs.self.String() || p.client != from:
		return fmt.Errorf("wrong parties")
	case !protocol.VerifySig(r.Client, r.Bytes(), sr.ClientSig):
		return fmt.Errorf("bad client signature")
	}
	if r.Model != p.model || r.Output != p.output || r.Prompt != p.prompt ||
		r.Completion != p.completion || r.Credits != p.credits {
		return fmt.Errorf("receipt does not match the job served")
	}
	sig, err := rs.key.Sign(r.Bytes())
	sr.ServerSig = sig
	return err
}

const PseudonymEpoch = 30 * 24 * time.Hour

// Key is this client's pseudonym toward server for the epoch containing t.
func Key(seed []byte, server peer.ID, t time.Time) (crypto.PrivKey, error) {
	mac := hmac.New(sha256.New, seed)
	fmt.Fprintf(mac, "bitllm receipt pseudonym v1|%s|%d", server, t.Unix()/int64(PseudonymEpoch/time.Second))
	return crypto.UnmarshalEd25519PrivateKey(ed25519.NewKeyFromSeed(mac.Sum(nil)))
}

// LoadSeed reads the receipt pseudonym seed, creating it on first run.
// Deleting the file breaks the link to past pseudonyms without changing the peer id.
// A file that exists but cannot be used is an error and is left as it is.
func LoadSeed(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("receipt seed %s is %d bytes, not 32: restore it from a backup, or delete it to start a new one", path, len(b))
		}
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot read receipt seed %s: %w", path, err)
	}
	b = make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, store.WriteFile(path, b, 0o600)
}
