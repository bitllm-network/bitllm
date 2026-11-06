// Package protocol is the wire contract every node shares: protocol ids,
// gossip topics, the announce message, and the bounds a stranger's message
// must pass before it is stored.
//
// Changing a name or a signed field here splits the network. Add a new
// version and keep accepting the old one for a release.
package protocol

import (
	"github.com/bitllm-network/bitllm/internal/models"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Protocol ids and gossip topics.
const (
	Rendezvous    = "bitllm/v1"
	AnnounceTopic = "bitllm/announce/1"
	// HelloProto answers with a node's current announce. It is how a node
	// that found a provider in the DHT learns its models at once, instead of
	// waiting for the next gossip round (up to an announce period).
	HelloProto = "/bitllm/hello/1.0.0"

	InferProto  = "/bitllm/infer/1.0.0"
	InferProto2 = "/bitllm/infer/1.1.0"

	ClaimTopic   = "bitllm/claims/1"
	MembersTopic = "bitllm/invites/1"
	ReceiptTopic = "bitllm/receipts/1"
	ReceiptProto = "/bitllm/receipt/1.0.0"
	SyncProto    = "/bitllm/receipts-sync/1.0.0"
)

// Gossip bounds. Peer ids cost nothing, so these do not stop a determined
// attacker alone; they keep gossip state from growing without limit.
const (
	MaxPeers          = 4096 // announcing peers we remember
	MaxAnnounceModels = 16
	MaxModelName      = 128
	MaxAnnounceSlots  = 256
	MaxLocJSON        = 16 // longest "loc" we accept from a peer (later versions may send other forms)
	MaxQueueDepth     = 64 // waiters one server holds; see the queue package
)

// Announce is what a serving node gossips about itself.
type Announce struct {
	Version string   `json:"v,omitempty"` // program version, so a future release can tell old nodes apart
	Models  []string `json:"models"`
	Free    int      `json:"free"`
	Max     int      `json:"max"`
	// Added in v0.2 (older nodes ignore them):
	Audit   []string          `json:"audit,omitempty"`   // models our server can score (prompt logprobs): we can be an auditor
	Weights map[string]string `json:"weights,omitempty"` // model -> hash of the weight files, as the operator states it
	// Added in v0.2.1:
	Loc string `json:"loc,omitempty"` // geohash cell (~156 km) for the node map, only if the operator set --location
	// Added in v0.3:
	Q int `json:"q,omitempty"` // requests waiting for a slot right now
	// Added in v0.4.5:
	Relay bool `json:"relay,omitempty"` // publicly reachable and running the relay service
}

// ValidAnnounce reports whether a is a well formed announce we would store.
func ValidAnnounce(a *Announce) bool {
	if len(a.Models) == 0 || len(a.Models) > MaxAnnounceModels ||
		a.Free < 0 || a.Max < 1 || a.Free > a.Max || a.Max > MaxAnnounceSlots || len(a.Version) > 64 ||
		len(a.Loc) > MaxLocJSON || a.Q < 0 || a.Q > MaxQueueDepth*64 {
		return false
	}
	served := map[string]bool{}
	for _, m := range a.Models {
		if m == "" || len(m) > MaxModelName {
			return false
		}
		served[m] = true
	}
	if len(a.Audit) > len(a.Models) || len(a.Weights) > len(a.Models) {
		return false
	}
	for _, m := range a.Audit {
		if !served[m] {
			return false
		}
	}
	for m, h := range a.Weights {
		if !served[m] || !models.ValidHash(h) {
			return false
		}
	}
	return true
}

// VerifySig checks a libp2p signature by the peer named id.
func VerifySig(id string, data, sig []byte) bool {
	p, err := peer.Decode(id)
	if err != nil {
		return false
	}
	pk, err := p.ExtractPublicKey()
	if err != nil {
		return false
	}
	ok, err := pk.Verify(data, sig)
	return err == nil && ok
}
