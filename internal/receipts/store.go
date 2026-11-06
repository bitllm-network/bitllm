package receipts

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitllm-org/bitllm/internal/store"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Retention and bounds. Two keys are enough to make a valid receipt, so what a
// node keeps must not depend on how many it is sent.
const (
	// Retention is how long a receipt is kept. A server's earliest receipt stays
	// past it, because the invite limit reads "has ever served" from it.
	Retention = 90 * 24 * time.Hour
	// futureSkew is how far ahead of our clock a receipt may be dated.
	futureSkew = 10 * time.Minute
	// compactOver is the share of dead lines that makes Open rewrite the file.
	compactOver = 0.20
	// maxRecord bounds one new receipt on disk; an honest one is under 1 KiB.
	maxRecord = 4 << 10
	// maxLine bounds a line read back from the file, which older versions wrote
	// without maxRecord.
	maxLine = 1 << 20
	// maxServers bounds the per-server table. Past it, servers we already know
	// keep their records and a new one is refused, so nobody's "has ever served"
	// can be pushed out by keys that cost nothing.
	maxServers = 50_000
	// maxStored bounds the receipts held for dedupe. Past it, only a receipt that
	// is a server's new earliest is stored.
	maxStored = 4_000_000
	// verifyChunk is how many lines Open checks at once.
	verifyChunk = 1024
)

// jobKey is a job id as dedupe remembers it, shorter than the id a signer chose.
type jobKey [16]byte

func keyOf(job string) jobKey {
	h := sha256.Sum256([]byte(job))
	return jobKey(h[:16])
}

// serverStats is what is kept per server in memory, the running counters that
// answer Tally, ServersSince and JobsSince without reading the receipts. The
// totals include receipts that are no longer on disk.
type serverStats struct {
	credits int64
	jobs    int
	last    int64 // newest TS counted
	through int64 // newest TS that was past retention when the file was last read

	first    int64 // earliest TS counted
	firstOff int64 // where the earliest receipt's line starts
	firstLen int32 // its length with the newline; 0 when it could not be written

	recent []int64 // TS of receipts within retention
	sorted bool
}

// foldedTotal is a server's sum of receipts compaction removed. It lives in the
// receipts file as a line of its own, so one rename replaces both together.
type foldedTotal struct {
	Server  string `json:"server"`
	Credits int64  `json:"credits"`
	Jobs    int    `json:"jobs"`
	Through int64  `json:"through"`
}

// fileLine is one line of the receipts file: a receipt or a folded total.
// Earlier versions skip a total line, since it holds no valid receipt.
type fileLine struct {
	SignedReceipt
	Total *foldedTotal `json:"total,omitempty"`
}

// Receipts is every valid receipt this node keeps, appended to a file, with the
// counters derived from them in memory. Reading the receipts themselves goes to
// the file, so memory does not grow with the history.
type Receipts struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	size    int64
	id      string
	seen    map[jobKey]struct{}
	servers map[string]*serverStats
	proven  map[string]int64 // server -> newest TS of a paid job we asked for
	pending map[string]*pendingJob
	rate    gossipRate
	key     crypto.PrivKey
	self    peer.ID
	seed    []byte
	mine    map[mineKey]string // our pseudonyms, see isOurs

	verified atomic.Int64 // signature checks Add has run
}

// Open loads the receipts file and checks every line again. A line that does not
// parse or verify is skipped. seed is this node's pseudonym seed, which tells
// which receipts are for jobs we asked for.
func Open(path string, key crypto.PrivKey, self peer.ID, seed []byte) (*Receipts, error) {
	rs := &Receipts{path: path, seen: map[jobKey]struct{}{}, servers: map[string]*serverStats{},
		proven: map[string]int64{}, pending: map[string]*pendingJob{}, key: key, self: self, seed: seed}
	rs.id = loadStoreID(idPath(path))
	now := time.Now()
	drop, dead, lines := rs.load(now)
	if lines > 0 && float64(dead+len(drop)) > compactOver*float64(lines) {
		if err := rs.compact(now, drop); err != nil {
			log.Printf("compact %s: %v (keeping it as it is)", path, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return rs, err
	}
	rs.f = f
	if fi, err := f.Stat(); err == nil {
		rs.size = fi.Size()
	}
	rs.endWithNewline()
	return rs, nil
}

func idPath(path string) string { return strings.TrimSuffix(path, ".jsonl") + ".id" }

// loadStoreID reads the store id, creating it when there is none. It is a name
// for this file's numbering, so losing it costs only a client's next sync
// starting from the beginning.
func loadStoreID(path string) string {
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 32 {
			return id
		}
	}
	return newStoreID(path)
}

func newStoreID(path string) string {
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	if err := store.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		log.Printf("store id %s: %v", path, err)
	}
	return id
}

// StoreID names this file's positions. It changes whenever they change meaning.
func (rs *Receipts) StoreID() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.id
}

// endWithNewline ends a last line a crash cut short, so the next append does not
// join it and get lost with it.
func (rs *Receipts) endWithNewline() {
	if rs.size == 0 {
		return
	}
	r, err := os.Open(rs.path)
	if err != nil {
		return
	}
	defer r.Close()
	var last [1]byte
	if _, err := r.ReadAt(last[:], rs.size-1); err == nil && last[0] != '\n' {
		if n, err := rs.f.Write([]byte{'\n'}); err == nil {
			rs.size += int64(n)
		}
	}
}

// scanLines calls fn with each line of r, where it starts and where the next one
// starts. A line over maxLine is passed as nil. The slice is valid only during the
// call.
func scanLines(r io.Reader, start int64, fn func(off, end int64, line []byte) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	off := start
	for {
		begin := off
		var long []byte
		tooLong := false
		var chunk []byte
		var err error
		for {
			chunk, err = br.ReadSlice('\n')
			off += int64(len(chunk))
			if err != bufio.ErrBufferFull {
				break
			}
			if tooLong = tooLong || len(long)+len(chunk) > maxLine; !tooLong {
				long = append(long, chunk...)
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		if off > begin {
			line := chunk
			switch {
			case tooLong || len(long)+len(chunk) > maxLine:
				line = nil
			case long != nil:
				line = append(long, chunk...)
			}
			if err := fn(begin, off, line); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// loadedLine is a receipt line waiting for its signatures to be checked.
type loadedLine struct {
	off, size int64
	sr        SignedReceipt
	ok        bool
}

// load reads the file and builds the counters. It returns the offsets of lines
// that hold nothing (garbage or duplicates), how many receipts are past retention
// and not their server's earliest, and how many lines there were.
func (rs *Receipts) load(now time.Time) (drop []int64, dead, lines int) {
	f, err := os.Open(rs.path)
	if err != nil {
		return nil, 0, 0
	}
	defer f.Close()
	cutoff := now.Add(-Retention).Unix()
	deadSeen := map[jobKey]struct{}{} // only for duplicates within the file
	var chunk []loadedLine
	flush := func() {
		store.ForEach(len(chunk), func(i int) { chunk[i].ok = chunk[i].sr.Valid() })
		for i := range chunk {
			l := &chunk[i]
			if !l.ok {
				drop = append(drop, l.off)
				continue
			}
			k := keyOf(l.sr.Receipt.Job)
			_, liveDup := rs.seen[k]
			_, deadDup := deadSeen[k]
			if liveDup || deadDup {
				drop = append(drop, l.off)
				continue
			}
			rs.count(&l.sr.Receipt, l.off, l.size, cutoff)
			if ts := l.sr.Receipt.TS; ts < cutoff {
				deadSeen[k] = struct{}{}
				st := rs.servers[l.sr.Receipt.Server]
				st.through = max(st.through, ts)
			} else {
				rs.seen[k] = struct{}{}
			}
			rs.noteOurs(&l.sr.Receipt, now)
		}
		chunk = chunk[:0]
	}
	err = scanLines(f, 0, func(off, end int64, line []byte) error {
		lines++
		if line == nil {
			drop = append(drop, off)
			return nil
		}
		var fl fileLine
		if json.Unmarshal(line, &fl) != nil {
			drop = append(drop, off)
			return nil
		}
		if fl.Total != nil {
			lines-- // not a record, and compaction writes it again
			rs.addFolded(fl.Total)
			return nil
		}
		chunk = append(chunk, loadedLine{off: off, size: int64(len(line)), sr: fl.SignedReceipt})
		if len(chunk) == verifyChunk {
			flush()
		}
		return nil
	})
	flush()
	if err != nil {
		log.Printf("read %s: %v", rs.path, err)
	}
	// A server's earliest receipt counts as kept, so it is not dead yet.
	for _, st := range rs.servers {
		if st.first < cutoff && st.firstLen > 0 {
			dead--
		}
	}
	return drop, dead + len(deadSeen), lines
}

// count adds one receipt to the counters.
func (rs *Receipts) count(r *Receipt, off, size int64, cutoff int64) {
	st := rs.servers[r.Server]
	if st == nil {
		st = &serverStats{first: r.TS, last: r.TS, firstOff: off, firstLen: int32(size), sorted: true}
		rs.servers[r.Server] = st
	} else {
		if r.TS < st.first || st.firstLen == 0 {
			st.first, st.firstOff, st.firstLen = min(r.TS, st.first), off, int32(size)
		}
		st.last = max(st.last, r.TS)
	}
	st.credits += r.Credits
	st.jobs++
	if r.TS < cutoff {
		return
	}
	if n := len(st.recent); n > 0 && st.recent[n-1] > r.TS {
		st.sorted = false
	}
	st.recent = append(st.recent, r.TS)
}

// addFolded adds a total line to the counters.
func (rs *Receipts) addFolded(t *foldedTotal) {
	if t.Server == "" {
		return
	}
	st := rs.servers[t.Server]
	if st == nil {
		st = &serverStats{first: t.Through, last: t.Through, sorted: true}
		rs.servers[t.Server] = st
	}
	st.credits += t.Credits
	st.jobs += t.Jobs
	st.through = max(st.through, t.Through)
	st.last = max(st.last, t.Through)
}

// compact rewrites the file with what is kept: receipts within retention, each
// server's earliest, and one total line per server for the rest. The new store
// id reaches the disk first, so a crash between the two can only make a client
// start over, never read old positions against the new file.
func (rs *Receipts) compact(now time.Time, drop []int64) error {
	in, err := os.Open(rs.path)
	if err != nil {
		return err
	}
	defer in.Close()
	sort.Slice(drop, func(a, b int) bool { return drop[a] < drop[b] })
	cutoff := now.Add(-Retention).Unix()
	folded := map[string]*foldedTotal{}
	fold := func(server string) *foldedTotal {
		t := folded[server]
		if t == nil {
			t = &foldedTotal{Server: server}
			folded[server] = t
		}
		return t
	}
	type place struct {
		off  int64
		size int32
	}
	firsts := map[string]place{}
	rs.id = newStoreID(idPath(rs.path))
	err = store.WriteStream(rs.path, 0o600, func(w io.Writer) error {
		out := bufio.NewWriterSize(w, 64<<10)
		var written int64
		err := scanLines(in, 0, func(off, end int64, line []byte) error {
			if i := sort.Search(len(drop), func(i int) bool { return drop[i] >= off }); i < len(drop) && drop[i] == off {
				return nil
			}
			if line == nil {
				return nil
			}
			var fl fileLine
			if json.Unmarshal(line, &fl) != nil {
				return nil
			}
			if t := fl.Total; t != nil {
				if t.Server != "" {
					to := fold(t.Server)
					to.Credits += t.Credits
					to.Jobs += t.Jobs
					to.Through = max(to.Through, t.Through)
				}
				return nil
			}
			r := &fl.Receipt
			st := rs.servers[r.Server]
			first := st != nil && st.firstLen > 0 && st.firstOff == off
			if r.TS < cutoff && !first {
				to := fold(r.Server)
				to.Credits += r.Credits
				to.Jobs++
				to.Through = max(to.Through, r.TS)
				return nil
			}
			begin := written
			n, err := out.Write(line)
			written += int64(n)
			if err == nil && line[len(line)-1] != '\n' {
				err = out.WriteByte('\n')
				written++
			}
			if first {
				firsts[r.Server] = place{begin, int32(written - begin)}
			}
			return err
		})
		in.Close() // before the rename, which an open file would block on some systems
		if err != nil {
			return err
		}
		servers := make([]string, 0, len(folded))
		for s := range folded {
			servers = append(servers, s)
		}
		sort.Strings(servers)
		enc := json.NewEncoder(out)
		for _, s := range servers {
			if err := enc.Encode(fileLine{Total: folded[s]}); err != nil {
				return err
			}
		}
		return out.Flush()
	})
	if err != nil {
		return err
	}
	for server, at := range firsts {
		st := rs.servers[server]
		st.firstOff, st.firstLen = at.off, at.size
	}
	return nil
}

// Add stores a receipt if it is valid and new; it reports whether it was. The
// dedupe comes first because a sync delivers mostly what we already have, and
// the signatures are the expensive part.
func (rs *Receipts) Add(sr SignedReceipt) bool {
	k := keyOf(sr.Receipt.Job)
	rs.mu.Lock()
	_, dup := rs.seen[k]
	rs.mu.Unlock()
	if dup {
		return false
	}
	rs.verified.Add(1)
	if !sr.Valid() {
		return false
	}
	line, err := json.Marshal(sr)
	if err != nil || len(line) > maxRecord {
		return false
	}
	line = append(line, '\n')
	r := &sr.Receipt
	now := time.Now()
	cutoff := now.Add(-Retention).Unix()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, dup := rs.seen[k]; dup {
		return false
	}
	rs.noteOurs(r, now)
	st := rs.servers[r.Server]
	switch {
	case st == nil && len(rs.servers) >= maxServers:
		return false
	case st == nil || r.TS < st.first: // a server's new earliest is always kept
	case r.TS < cutoff && r.TS <= st.through:
		// Past retention, at or before the last one folded: it may be in the
		// totals already and nothing here can tell, so it does not count again.
		return false
	case len(rs.seen) >= maxStored:
		return false
	}
	rs.seen[k] = struct{}{}
	off, size := rs.size, int64(len(line))
	if rs.f == nil {
		size = 0
	} else if n, err := rs.f.Write(line); err != nil {
		log.Printf("write %s: %v", rs.path, err)
		rs.size += int64(n)
		size = 0
	} else {
		rs.size += size
	}
	rs.count(r, off, size, cutoff)
	return true
}

// AddSynced is Add for a receipt from a sync, which has no rate or age limit but
// may not be dated ahead of our clock by more than gossip allows. Old is fine:
// delivering history is what sync is for.
func (rs *Receipts) AddSynced(sr SignedReceipt) bool {
	if sr.Receipt.TS > time.Now().Add(futureSkew).Unix() {
		return false
	}
	return rs.Add(sr)
}

// Close closes the file. What is already held stays readable. A receipt added
// after this is kept in memory only, and the next start syncs it again.
func (rs *Receipts) Close() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.f != nil {
		rs.f.Close()
		rs.f = nil
	}
}

// Flush asks the disk to hold what was appended.
func (rs *Receipts) Flush() error {
	rs.mu.Lock()
	f := rs.f
	rs.mu.Unlock()
	if f == nil {
		return nil
	}
	return f.Sync()
}

// Tally sums credits per server, including receipts past retention.
func (rs *Receipts) Tally() map[string]ServerCredits {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make(map[string]ServerCredits, len(rs.servers))
	for s, st := range rs.servers {
		out[s] = ServerCredits{Credits: st.credits, Jobs: st.jobs}
	}
	return out
}

// ServersSince reports, per server, whether it served a job at or after since.
// Every server that ever served has an entry, whatever the value.
func (rs *Receipts) ServersSince(since int64) map[string]bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make(map[string]bool, len(rs.servers))
	for s, st := range rs.servers {
		out[s] = st.last >= since
	}
	return out
}

// JobsSince counts jobs per server at or after since, for since within retention.
func (rs *Receipts) JobsSince(since int64) map[string]int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := map[string]int{}
	for s, st := range rs.servers {
		if !st.sorted {
			sort.Slice(st.recent, func(a, b int) bool { return st.recent[a] < st.recent[b] })
			st.sorted = true
		}
		i := sort.Search(len(st.recent), func(i int) bool { return st.recent[i] >= since })
		if n := len(st.recent) - i; n > 0 {
			out[s] = n
		}
	}
	return out
}

// mineKey names one of our pseudonyms.
type mineKey struct {
	server string
	epoch  int64
}

// maxMine bounds the cache of derived pseudonyms; it starts over when full.
const maxMine = 4096

// pseudonym is our client id toward server in the epoch containing ts.
func (rs *Receipts) pseudonym(server string, ts int64) string {
	k := mineKey{server, ts / int64(PseudonymEpoch/time.Second)}
	if id, ok := rs.mine[k]; ok {
		return id
	}
	p, err := peer.Decode(server)
	if err != nil {
		return ""
	}
	key, err := Key(rs.seed, p, time.Unix(ts, 0))
	if err != nil {
		return ""
	}
	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return ""
	}
	if rs.mine == nil || len(rs.mine) >= maxMine {
		rs.mine = map[mineKey]string{}
	}
	rs.mine[k] = id.String()
	return id.String()
}

// isOurs reports whether r is signed by our pseudonym for its server, for the
// epoch of its TS or the one before. Only our seed can make that key, so a
// receipt that passes it records a job this node really asked for. Whoever else
// signs a receipt, however it is dated, does not.
func (rs *Receipts) isOurs(r *Receipt) bool {
	if len(rs.seed) == 0 {
		return false
	}
	epoch := int64(PseudonymEpoch / time.Second)
	return r.Client == rs.pseudonym(r.Server, r.TS) || r.Client == rs.pseudonym(r.Server, r.TS-epoch)
}

// noteOurs records a paid job we asked for. Only our own receipts enter this map
// so it needs no cap, and an old receipt is skipped before the key derivation it
// could no longer earn anything with.
func (rs *Receipts) noteOurs(r *Receipt, now time.Time) {
	if r.Credits <= 0 || r.TS < now.Add(-provenWindow).Unix() || !rs.isOurs(r) {
		return
	}
	rs.proven[r.Server] = max(rs.proven[r.Server], r.TS)
}

// provenWindow is how old a receipt can be and still matter to Proven, whose
// callers ask about the last 7 days.
const provenWindow = 8 * 24 * time.Hour

// Proven returns the servers that did a paid job this node asked for, at or after
// since. Unlike the public tallies this cannot be claimed by anyone else.
func (rs *Receipts) Proven(since int64) map[string]bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := map[string]bool{}
	for s, ts := range rs.proven {
		if ts >= since {
			out[s] = true
		}
	}
	return out
}

// isDead reports whether the line at off is past retention and not kept for its
// server. The caller holds no lock.
func (rs *Receipts) isDead(r *Receipt, off, cutoff int64) bool {
	if r.TS >= cutoff {
		return false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	st := rs.servers[r.Server]
	return st == nil || st.firstOff != off
}

// Stream sends raw receipt lines from position since, skipping what retention no
// longer keeps. It stops after at most limit lines or budget bytes and returns
// the position to continue from and whether anything is left. A cursor made
// against another store id, or past the end of this file, starts from the
// beginning. The cursor is a byte offset in this file, which no signer can
// influence; compaction changes offsets, so it also changes the store id.
func (rs *Receipts) Stream(since int64, cursorID string, limit int, budget int64, emit func(line []byte) error) (cursor int64, more bool, err error) {
	rs.mu.Lock()
	size, id := rs.size, rs.id
	rs.mu.Unlock()
	if cursorID != id || since < 0 || since > size {
		since = 0
	}
	f, err := os.Open(rs.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return since, false, err
	}
	defer f.Close()
	cutoff := time.Now().Add(-Retention).Unix()
	cursor = since
	sent := 0
	errStop := errors.New("stop")
	err = scanLines(io.NewSectionReader(f, since, size-since), since, func(off, end int64, line []byte) error {
		if line != nil {
			var fl fileLine
			if json.Unmarshal(line, &fl) == nil && fl.Total == nil && fl.Receipt.Job != "" && !rs.isDead(&fl.Receipt, off, cutoff) {
				if sent >= limit || int64(len(line)) > budget {
					more = true
					return errStop
				}
				if err := emit(line); err != nil {
					return err
				}
				sent++
				budget -= int64(len(line))
			}
		}
		cursor = end
		return nil
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	return cursor, more, err
}

// StreamRecent sends what the unversioned sync sends: each server's earliest
// receipt, which keeps "has ever served" whole, then the last recent lines.
func (rs *Receipts) StreamRecent(recent int, budget int64, emit func(line []byte) error) error {
	rs.mu.Lock()
	size := rs.size
	firsts := make(map[int64]int32, len(rs.servers))
	for _, st := range rs.servers {
		if st.firstLen > 0 {
			firsts[st.firstOff] = st.firstLen
		}
	}
	rs.mu.Unlock()
	f, err := os.Open(rs.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	offs := make([]int64, 0, len(firsts))
	for off := range firsts {
		offs = append(offs, off)
	}
	sort.Slice(offs, func(a, b int) bool { return offs[a] < offs[b] })
	for _, off := range offs {
		n := firsts[off]
		if int64(n) > budget {
			return nil
		}
		line := make([]byte, n)
		if _, err := f.ReadAt(line, off); err != nil {
			continue
		}
		if err := emit(line); err != nil {
			return err
		}
		budget -= int64(n)
	}
	start, err := tailOffset(f, size, recent)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-Retention).Unix()
	errStop := errors.New("stop")
	err = scanLines(io.NewSectionReader(f, start, size-start), start, func(off, end int64, line []byte) error {
		if _, first := firsts[off]; first || line == nil {
			return nil
		}
		var fl fileLine
		if json.Unmarshal(line, &fl) != nil || fl.Total != nil || fl.Receipt.Job == "" || fl.Receipt.TS < cutoff {
			return nil
		}
		if int64(len(line)) > budget {
			return errStop
		}
		budget -= int64(len(line))
		return emit(line)
	})
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

// tailOffset finds where the last n lines of the first size bytes start.
func tailOffset(f io.ReaderAt, size int64, n int) (int64, error) {
	const block = 256 << 10
	buf := make([]byte, block)
	lines := 0
	for end := size; end > 0; {
		start := max(end-block, 0)
		chunk := buf[:end-start]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return 0, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] != '\n' || start+int64(i) == size-1 {
				continue
			}
			if lines++; lines == n {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

// All returns every receipt the file keeps. It reads the whole file; sync uses
// Stream.
func (rs *Receipts) All() []SignedReceipt {
	var out []SignedReceipt
	rs.Stream(0, rs.StoreID(), math.MaxInt, math.MaxInt64, func(line []byte) error {
		var sr SignedReceipt
		if json.Unmarshal(line, &sr) == nil {
			out = append(out, sr)
		}
		return nil
	})
	return out
}
