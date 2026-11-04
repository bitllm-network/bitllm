package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/bitllm-network/bitllm/internal/netmap"
	"github.com/bitllm-network/bitllm/internal/protocol"
)

// GET /network aggregates what this node hears: per model and per cell, node
// counts and free slots. It names no peer ids, so the public map cannot tie a
// node's identity to its region. bitllm.org serves a copy of the bootstrap
// node's answer (scripts/network-json.sh).

// networkSnapshot aggregates the live announces (and our own, if we serve).
func (n *Node) networkSnapshot() netmap.View {
	anns := []protocol.Announce{}
	for _, a := range n.alive() {
		anns = append(anns, a)
	}
	if own := n.ownAnnounce(); own != nil {
		anns = append(anns, *own)
	}
	return netmap.Aggregate(anns, time.Now().UTC())
}

// ownAnnounce is what we last gossiped, or nil if we serve nothing right now.
func (n *Node) ownAnnounce() *protocol.Announce {
	a := n.lastAnn.Load()
	if a == nil || len(a.Models) == 0 {
		return nil
	}
	return a
}

func (n *Node) network(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n.networkSnapshot())
}
