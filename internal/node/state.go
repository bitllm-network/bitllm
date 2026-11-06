package node

import "log"

// wireState connects the stores to each other once they all exist.
func (n *Node) wireState() {
	n.claims.SetSelf(n.host.ID().String())
	if n.members.Enabled() {
		n.claims.SetMembership(func(owner string) bool { return n.memberSet().Member[owner] })
	}
}

// flushState writes every store's pending changes now, for a clean shutdown.
func (n *Node) flushState() {
	n.saveState(n.members.Flush, n.claims.Flush, n.ledger.Flush, n.quota.Flush, n.receipts.Flush)
}

// closeState is flushState that also stops the stores' background writes, so
// nothing touches the data directory afterward.
func (n *Node) closeState() {
	n.saveState(n.members.Close, n.claims.Close, n.ledger.Close, n.quota.Close, n.receipts.Flush)
}

func (n *Node) saveState(steps ...func() error) {
	for _, step := range steps {
		if err := step(); err != nil {
			log.Printf("save state: %v", err)
		}
	}
}
