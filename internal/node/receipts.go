package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/bitllm-network/bitllm/internal/account"
	"github.com/bitllm-network/bitllm/internal/members"
	"github.com/bitllm-network/bitllm/internal/protocol"
	"github.com/bitllm-network/bitllm/internal/receipts"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// handleReceipt: a client hands us its signed receipt for a job we served.
func (n *Node) handleReceipt(s network.Stream) {
	defer s.Close()
	var sr receipts.SignedReceipt
	if err := json.NewDecoder(io.LimitReader(s, 64<<10)).Decode(&sr); err != nil {
		return
	}
	if err := n.receipts.Countersign(&sr, s.Conn().RemotePeer()); err != nil {
		log.Printf("receipt from %s rejected: %v", s.Conn().RemotePeer(), err)
		fmt.Fprintf(s, "%s\n", err)
		return
	}
	n.receipts.Add(sr)
	b, _ := json.Marshal(sr)
	n.receiptTop.Publish(context.Background(), b)
	s.Write([]byte("ok\n"))
}

// sendReceipt signs a receipt for a job p served us and gives it to p. It
// signs with its pseudonym for p, so the gossiped receipt does not name this node.
func (n *Node) sendReceipt(p peer.ID, job, model string, m meter) error {
	key, err := receipts.Key(n.receiptSeed, p, time.Now())
	if err != nil {
		return err
	}
	client, _ := peer.IDFromPrivateKey(key)
	prompt, completion := m.tokens()
	r := receipts.Receipt{Job: job, Model: model, Client: client.String(), Server: p.String(),
		Prompt: prompt, Completion: completion, Credits: m.credits(), Output: m.sum(), TS: time.Now().Unix()}
	sig, err := key.Sign(r.Bytes())
	if err != nil {
		return err
	}
	sr := receipts.SignedReceipt{Receipt: r, ClientSig: sig}
	// A client can see the last SSE event and hang up before the server's
	// upstream body ends, so the job may not be in the server's pending set
	// yet when the receipt arrives. Wait a little for it to be registered.
	for attempt := 0; ; attempt++ {
		err = n.deliverReceipt(p, sr)
		if err == nil || !errors.Is(err, receipts.ErrUnknownJob) || attempt == 5 {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (n *Node) deliverReceipt(p peer.ID, sr receipts.SignedReceipt) error {
	// A receipt is one short line: give a stalled peer ten seconds, then let
	// the retry above open a fresh stream instead of blocking here.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := n.host.NewStream(network.WithAllowLimitedConn(ctx, "receipt"), p, protocol.ReceiptProto)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := json.NewEncoder(s).Encode(sr); err != nil {
		return err
	}
	s.CloseWrite()
	line, _ := bufio.NewReader(s).ReadString('\n')
	switch line {
	case "ok\n":
		return nil
	case receipts.ErrUnknownJob.Error() + "\n":
		return receipts.ErrUnknownJob
	}
	return fmt.Errorf("server refused receipt: %q", line)
}

// syncItem is one line of the sync stream: a receipt, or a node claim.
type syncItem struct {
	receipts.SignedReceipt
	Claim  *account.Claim  `json:"claim,omitempty"`
	Invite *members.Invite `json:"invite,omitempty"`
	Revoke *members.Revoke `json:"revoke,omitempty"`
}

// handleSync streams every receipt and claim we know, so a new node can catch up.
func (n *Node) handleSync(s network.Stream) {
	defer s.Close()
	enc := json.NewEncoder(s)
	for _, sr := range n.receipts.All() {
		if enc.Encode(sr) != nil {
			return
		}
	}
	for _, c := range n.claims.All() {
		if enc.Encode(map[string]any{"claim": c}) != nil {
			return
		}
	}
	for _, it := range n.members.Items() {
		if enc.Encode(it) != nil {
			return
		}
	}
}

func (n *Node) syncReceipts(ctx context.Context, p peer.ID) (int, error) {
	s, err := n.host.NewStream(network.WithAllowLimitedConn(ctx, "sync"), p, protocol.SyncProto)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	s.CloseWrite()
	dec, added := json.NewDecoder(s), 0
	for {
		var it syncItem
		if err := dec.Decode(&it); err != nil {
			if err == io.EOF {
				err = nil
			}
			return added, err
		}
		if it.Claim != nil {
			n.claims.Add(*it.Claim)
		} else if it.Invite != nil || it.Revoke != nil {
			if n.members.Add(members.Item{Invite: it.Invite, Revoke: it.Revoke}) {
				n.invalidateMembers()
			}
		} else if n.receipts.Add(it.SignedReceipt) {
			added++
		}
	}
}

func (n *Node) readReceipts(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		var sr receipts.SignedReceipt
		if json.Unmarshal(msg.Data, &sr) == nil {
			n.receipts.Add(sr)
		}
	}
}

// creditsTable is the tally grouped by owner, for the gateway.
func (n *Node) creditsTable() []account.OwnerCredits {
	return account.OwnerTable(n.receipts.Tally(), n.claims)
}
