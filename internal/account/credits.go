package account

import (
	"sort"

	"github.com/bitllm-network/bitllm/internal/receipts"
)

// OwnerCredits is the credits table row for one owner (or one unclaimed node).
type OwnerCredits struct {
	Owner   string          `json:"owner,omitempty"`
	Name    string          `json:"name,omitempty"`
	Credits int64           `json:"credits"`
	Jobs    int             `json:"jobs"`
	Nodes   []NodeCreditRow `json:"nodes"`
}

type NodeCreditRow struct {
	Node    string `json:"node"`
	Credits int64  `json:"credits"`
	Jobs    int    `json:"jobs"`
}

// OwnerTable groups per-node credits by each node's current owner.
func OwnerTable(perNode map[string]receipts.ServerCredits, claims *Claims) []OwnerCredits {
	rows := map[string]*OwnerCredits{}
	for node, c := range perNode {
		key, owner, name := "node:"+node, "", ""
		if cl, ok := claims.Get(node); ok {
			key, owner, name = cl.Owner, ChecksumAddress(cl.Owner), cl.Name
		}
		r := rows[key]
		if r == nil {
			r = &OwnerCredits{Owner: owner}
			rows[key] = r
		}
		if name != "" {
			r.Name = name
		}
		r.Credits += c.Credits
		r.Jobs += c.Jobs
		r.Nodes = append(r.Nodes, NodeCreditRow{node, c.Credits, c.Jobs})
	}
	out := make([]OwnerCredits, 0, len(rows))
	for _, r := range rows {
		sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].Credits > r.Nodes[j].Credits })
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Credits > out[j].Credits })
	return out
}
