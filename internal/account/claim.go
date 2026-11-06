package account

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Claim says that node belongs to owner, under a display name.
type Claim struct {
	Node     string `json:"node"`
	Owner    string `json:"owner"` // lowercase 0x address
	Name     string `json:"name"`
	Time     int64  `json:"time"`
	OwnerSig string `json:"owner_sig"` // hex, personal_sign of Message()
	NodeSig  []byte `json:"node_sig"`  // libp2p signature of Message()
}

// Message is the exact text the owner's wallet signs.
func (c *Claim) Message() string {
	return fmt.Sprintf("BitLLM node claim\nnode: %s\nowner: %s\nname: %s\ntime: %d", c.Node, c.Owner, c.Name, c.Time)
}

func validName(s string) bool {
	return len(s) <= 32 && !strings.ContainsAny(s, "\n\r")
}

func (c *Claim) Valid() bool {
	if !validName(c.Name) || c.Time > time.Now().Add(10*time.Minute).Unix() {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(c.OwnerSig, "0x"))
	if err != nil {
		return false
	}
	addr, err := EthRecover(c.Message(), sig)
	if err != nil || addr != c.Owner {
		return false
	}
	return protocol.VerifySig(c.Node, []byte(c.Message()), c.NodeSig)
}

// NewClaim builds an unsigned claim of this node for owner.
func NewClaim(self peer.ID, owner, name string) (*Claim, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if len(owner) != 42 || !strings.HasPrefix(owner, "0x") {
		return nil, fmt.Errorf("owner must be a 0x address")
	}
	if _, err := hex.DecodeString(owner[2:]); err != nil {
		return nil, fmt.Errorf("owner must be a 0x address")
	}
	name = strings.TrimSpace(name)
	if !validName(name) {
		return nil, fmt.Errorf("name: one line, at most 32 bytes")
	}
	return &Claim{Node: self.String(), Owner: owner, Name: name, Time: time.Now().Unix()}, nil
}

// SignClaimAsNode adds the node's signature after checking the owner's.
func SignClaimAsNode(c *Claim, nodeKey crypto.PrivKey) error {
	sig, err := hex.DecodeString(strings.TrimPrefix(c.OwnerSig, "0x"))
	if err != nil {
		return fmt.Errorf("owner signature is not hex")
	}
	if addr, err := EthRecover(c.Message(), sig); err != nil || addr != c.Owner {
		return fmt.Errorf("owner signature does not match %s", c.Owner)
	}
	c.NodeSig, err = nodeKey.Sign([]byte(c.Message()))
	return err
}
