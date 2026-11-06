package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/members"
	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/bitllm-org/bitllm/internal/receipts"
	"github.com/bitllm-org/bitllm/internal/store"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2pproto "github.com/libp2p/go-libp2p/core/protocol"
)

// Limits on serving a sync, which any peer may ask for.
const (
	syncMaxActive    = 4                // syncs served at once
	syncPeerEvery    = time.Minute      // per peer, unless the last answer said more
	syncWriteStall   = 30 * time.Second // a peer must keep reading
	syncMaxDuration  = 5 * time.Minute  // and finish: a slow reader may not keep a slot
	syncMaxResponse  = 64 << 20         // bytes in one response
	syncMaxReceipts  = 20000            // receipts in one response
	syncRequestBytes = 4 << 10
	syncRequestWait  = 5 * time.Second
	syncMaxItem      = 256 << 10 // one line, as a client reads it
	syncGatePeers    = 4096      // peers the rate limit remembers
	syncMaxCursors   = 256       // seeds the client keeps a cursor for
)

// syncItem is one line of the sync stream: a receipt, a claim, a membership item
// or, on sync 2.0.0, the closing line.
type syncItem struct {
	receipts.SignedReceipt
	Claim  *account.Claim  `json:"claim,omitempty"`
	Invite *members.Invite `json:"invite,omitempty"`
	Revoke *members.Revoke `json:"revoke,omitempty"`
	syncEnd
}

// syncEnd closes a sync 2.0.0 response: where to continue, and whether to go on
// now. The cursor means nothing outside the store id it came with.
type syncEnd struct {
	Cursor  *int64 `json:"cursor,omitempty"`
	More    bool   `json:"more,omitempty"`
	StoreID string `json:"store_id,omitempty"`
}

// syncRequest opens a sync 2.0.0 stream.
type syncRequest struct {
	Since    int64  `json:"since"`
	CursorID string `json:"cursor_id"`
}

// syncGate decides whether a peer's sync may run now.
type syncGate struct {
	mu     sync.Mutex
	active map[peer.ID]bool
	last   map[peer.ID]syncVisit

	// Tests lower these. Zero means the constant above.
	maxBusy     int
	every       time.Duration
	stall       time.Duration
	maxReceipts int
}

type syncVisit struct {
	at   time.Time
	more bool
}

func (g *syncGate) enter(p peer.ID, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == nil {
		g.active, g.last = map[peer.ID]bool{}, map[peer.ID]syncVisit{}
	}
	busy, every := g.maxBusy, g.every
	if busy == 0 {
		busy = syncMaxActive
	}
	if every == 0 {
		every = syncPeerEvery
	}
	if g.active[p] || len(g.active) >= busy {
		return false
	}
	if v, ok := g.last[p]; ok && !v.more && now.Sub(v.at) < every {
		return false
	}
	if _, ok := g.last[p]; !ok && len(g.last) >= syncGatePeers {
		for q, v := range g.last {
			if now.Sub(v.at) >= every {
				delete(g.last, q)
			}
		}
		if len(g.last) >= syncGatePeers {
			return false
		}
	}
	g.active[p] = true
	g.last[p] = syncVisit{at: now}
	return true
}

func (g *syncGate) receipts() int {
	if g.maxReceipts > 0 {
		return g.maxReceipts
	}
	return syncMaxReceipts
}

func (g *syncGate) leave(p peer.ID, more bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.active, p)
	if v, ok := g.last[p]; ok {
		v.more = more
		g.last[p] = v
	}
}

// syncWriter writes to a sync stream with a rolling deadline: each piece has its
// own, so what counts is that the peer keeps reading.
type syncWriter struct {
	s     network.Stream
	stall time.Duration
}

func (w syncWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), 64<<10)]
		w.s.SetWriteDeadline(time.Now().Add(w.stall))
		n, err := w.s.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// encodeLines is the given values as one JSON line each.
func encodeLines[T any](values []T, wrap func(T) any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, v := range values {
		enc.Encode(wrap(v))
	}
	return b.Bytes()
}

func (n *Node) claimLines() []byte {
	return encodeLines(n.claims.All(), func(c account.Claim) any { return map[string]any{"claim": c} })
}

func (n *Node) memberLines() []byte {
	return encodeLines(n.members.Items(), func(it members.Item) any { return it })
}

// serveSync runs one served sync with every limit in place. Over a limit the
// stream closes with no data.
func (n *Node) serveSync(s network.Stream, respond func(w io.Writer) (more bool, err error)) {
	defer s.Close()
	p := s.Conn().RemotePeer()
	if !n.syncGate.enter(p, time.Now()) {
		return
	}
	more := false
	defer func() { n.syncGate.leave(p, more) }()
	stop := time.AfterFunc(syncMaxDuration, func() { s.Reset() })
	defer stop.Stop()
	stall := n.syncGate.stall
	if stall == 0 {
		stall = syncWriteStall
	}
	var err error
	if more, err = respond(syncWriter{s, stall}); err != nil {
		more = false
		s.Reset()
	}
}

// handleSync serves the unversioned sync older clients speak, in the order they
// expect: receipts, claims, invites. Their reader checks an invite against who
// has served, so receipts come first; claims and invites always arrive whole,
// which leaves the receipts whatever room they do not need. Each server's earliest
// receipt goes first since "has ever served" reads only that one.
func (n *Node) handleSync(s network.Stream) {
	n.serveSync(s, func(w io.Writer) (bool, error) {
		claims, items := n.claimLines(), n.memberLines()
		budget := syncMaxResponse - int64(len(claims)+len(items))
		if err := n.receipts.StreamRecent(n.syncGate.receipts(), budget, func(line []byte) error {
			_, err := w.Write(line)
			return err
		}); err != nil {
			return false, err
		}
		if _, err := w.Write(claims); err != nil {
			return false, err
		}
		_, err := w.Write(items)
		return false, err
	})
}

// handleSync2 serves sync 2.0.0. Membership and claims come first and whole,
// because they are small and matter most; then receipts from the client's cursor,
// up to the limits, and the closing line. The client reads one request line first.
func (n *Node) handleSync2(s network.Stream) {
	s.SetReadDeadline(time.Now().Add(syncRequestWait))
	line, err := bufio.NewReaderSize(io.LimitReader(s, syncRequestBytes), syncRequestBytes).ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		s.Close()
		return
	}
	var req syncRequest
	if json.Unmarshal(line, &req) != nil {
		s.Close()
		return
	}
	s.SetReadDeadline(time.Time{})
	n.serveSync(s, func(w io.Writer) (bool, error) {
		items, claims := n.memberLines(), n.claimLines()
		if _, err := w.Write(items); err != nil {
			return false, err
		}
		if _, err := w.Write(claims); err != nil {
			return false, err
		}
		budget := syncMaxResponse - int64(len(items)+len(claims))
		cursor, more, err := n.receipts.Stream(req.Since, req.CursorID, n.syncGate.receipts(), budget, func(line []byte) error {
			_, err := w.Write(line)
			return err
		})
		if err != nil {
			return false, err
		}
		end, _ := json.Marshal(syncEnd{Cursor: &cursor, More: more, StoreID: n.receipts.StoreID()})
		_, err = w.Write(append(end, '\n'))
		return more, err
	})
}

// syncCursors is where each seed's sync left off, in a small file.
type syncCursors struct {
	mu   sync.Mutex
	path string
	by   map[string]syncCursor
}

type syncCursor struct {
	Since int64  `json:"since"`
	ID    string `json:"cursor_id"`
}

func openSyncCursors(path string) *syncCursors {
	c := &syncCursors{path: path, by: map[string]syncCursor{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	if err := json.Unmarshal(b, &c.by); err != nil || c.by == nil {
		log.Printf("sync cursors %s: %v (starting over)", path, err)
		c.by = map[string]syncCursor{}
	}
	return c
}

func (c *syncCursors) get(p peer.ID) syncCursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by[p.String()]
}

func (c *syncCursors) set(p peer.ID, cur syncCursor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.by[p.String()]; !ok && len(c.by) >= syncMaxCursors {
		c.by = map[string]syncCursor{} // a cursor is only a saving; losing one is safe
	}
	c.by[p.String()] = cur
	b, _ := json.Marshal(c.by)
	if err := store.WriteFile(c.path, b, 0o600); err != nil {
		log.Printf("sync cursors: %v", err)
	}
}

// syncReceipts brings what one seed knows into our stores. It asks for sync 2.0.0
// and takes what the seed offers, so a seed that knows only the old id serves the
// old sync on the same stream. On 2.0.0 it continues while the seed says more, all
// within SyncLimit, and starts next time where this one stopped.
func (n *Node) syncReceipts(ctx context.Context, p peer.ID) (added int, err error) {
	ctx, cancel := context.WithTimeout(ctx, n.cfg.SyncLimit)
	defer cancel()
	for {
		cur := n.syncCursors.get(p)
		got, end, err := n.syncOnce(ctx, p, cur)
		added += got
		if err != nil || end == nil {
			return added, err
		}
		next := syncCursor{Since: *end.Cursor, ID: end.StoreID}
		n.syncCursors.set(p, next)
		if !end.More || next == cur {
			return added, nil
		}
	}
}

// syncOnce reads one response. A seed may answer slowly but may not go silent or
// run on forever: the read deadline restarts with each item, the context bounds
// the whole sync, and a sync that fails resets the stream. It returns the closing
// line, which only sync 2.0.0 has.
func (n *Node) syncOnce(ctx context.Context, p peer.ID, cur syncCursor) (added int, end *syncEnd, err error) {
	open, cancelOpen := context.WithTimeout(ctx, n.cfg.SyncStall)
	defer cancelOpen()
	s, err := n.host.NewStream(network.WithAllowLimitedConn(open, "sync"), p, protocol.SyncProto2, protocol.SyncProto)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil {
			s.Reset()
		} else {
			s.Close()
		}
	}()
	// The context does not reach a read that is already blocked; this does.
	stop := context.AfterFunc(ctx, func() { s.Reset() })
	defer stop()
	s.SetDeadline(time.Now().Add(n.cfg.SyncStall))
	v2 := s.Protocol() == libp2pproto.ID(protocol.SyncProto2)
	if v2 {
		req, _ := json.Marshal(syncRequest{Since: cur.Since, CursorID: cur.ID})
		if _, err := s.Write(append(req, '\n')); err != nil {
			return 0, nil, err
		}
	}
	s.CloseWrite()

	batch := syncBatch{n: n}
	defer func() {
		// What did arrive is still good, even from a sync that failed.
		batch.apply()
		if err == nil {
			log.Printf("sync from %s over %s: %d new receipts", p, s.Protocol(), added)
		}
	}()
	br := bufio.NewReaderSize(s, syncMaxItem+1)
	var total int64
	for {
		s.SetReadDeadline(time.Now().Add(n.cfg.SyncStall))
		line, err := br.ReadSlice('\n')
		switch {
		case err == bufio.ErrBufferFull:
			return added, nil, fmt.Errorf("sync item over %d bytes", syncMaxItem)
		case err == io.EOF && len(bytes.TrimSpace(line)) == 0:
			if v2 {
				return added, nil, errors.New("seed closed the sync without finishing it")
			}
			return added, nil, nil
		case err != nil && err != io.EOF:
			return added, nil, err
		}
		if total += int64(len(line)); v2 && total > syncMaxResponse+syncMaxItem {
			return added, nil, errors.New("sync response too large")
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var it syncItem
		if err := json.Unmarshal(line, &it); err != nil {
			return added, nil, err
		}
		switch {
		case it.Cursor != nil:
			if v2 {
				return added, &it.syncEnd, nil
			}
		case it.Claim != nil:
			batch.addClaim(*it.Claim, len(line))
		case it.Invite != nil || it.Revoke != nil:
			batch.addItem(members.Item{Invite: it.Invite, Revoke: it.Revoke}, len(line))
		case n.receipts.AddSynced(it.SignedReceipt):
			added++
		}
	}
}

// syncBatchBytes is how much of a sync waits in memory to be applied together.
const syncBatchBytes = 64 << 20

// syncBatch holds claims and membership items until the stream ends. Whether an
// invite counts can depend on who has served, which takes the receipts and claims
// of the same response, and where a claim belongs depends on the invites. So they
// apply in one step, in that order, whatever order the seed sent them in.
type syncBatch struct {
	n      *Node
	claims []account.Claim
	items  []members.Item
	bytes  int
}

func (b *syncBatch) addClaim(c account.Claim, size int) {
	b.claims = append(b.claims, c)
	b.grew(size)
}

func (b *syncBatch) addItem(it members.Item, size int) {
	if b.n.members.Known(it) {
		return
	}
	b.items = append(b.items, it)
	b.grew(size)
}

func (b *syncBatch) grew(size int) {
	if b.bytes += size; b.bytes >= syncBatchBytes {
		b.apply()
	}
}

func (b *syncBatch) apply() {
	n := b.n
	claims, items := b.claims, b.items
	b.claims, b.items, b.bytes = nil, nil, 0
	stored := n.claims.AddBatch(claims)
	if stored > 0 || len(items) > 0 {
		n.dropCachedSets() // the invites below read who has served, as of now
	}
	changed := false
	for _, ok := range n.members.AddBatch(items) {
		changed = changed || ok
	}
	if changed || stored > 0 {
		n.invalidateMembers()
	}
}
