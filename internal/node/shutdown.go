package node

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"
)

// Drain is how long a stopping node gives live gateway requests and peer jobs
// to finish before cutting them.
const Drain = 30 * time.Second

// Shutdown stops a node in order: it stops announcing and taking peer jobs,
// drains srv and then the peer jobs until ctx ends, closes srv, and closes the
// node. Nothing is published after the drain.
func (n *Node) Shutdown(ctx context.Context, srv *http.Server) error {
	n.stopIntake()
	err := srv.Shutdown(ctx)
	srv.Close()
	return errors.Join(err, n.Close(ctx))
}

// stopIntake ends what brings new work: handleInfer refuses while closing is
// set, and the loops that announce this node or publish its claim stop here.
func (n *Node) stopIntake() {
	n.closing.Store(true)
	n.stopLoops()
}

// Close waits for peer jobs until ctx ends, closes the host, runs closers, and
// ends everything still tied to the node. Only the first call does the work.
func (n *Node) Close(ctx context.Context) error {
	n.closeOnce.Do(func() {
		n.stopIntake()
		n.waitIdle(ctx)
		if !n.Idle() {
			log.Printf("shutdown: cutting work still running after the drain")
		}
		n.closeErr = n.host.Close()
		for _, f := range n.closers {
			f()
		}
		n.stopAll()
	})
	return n.closeErr
}

// waitIdle returns when no job, waiter or gateway request is left, or ctx ends.
func (n *Node) waitIdle(ctx context.Context) {
	for !n.Idle() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
