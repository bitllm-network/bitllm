package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/bitllm-org/bitllm/internal/receipts"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	p2pproto "github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multistream"
)

// InferProto: the client writes one JSON line {path, body}, then closes its
// write side. The server answers with one JSON header line {status, error},
// then (if status is 200) the upstream response body verbatim: SSE lines for
// stream requests, a JSON document otherwise.
//
// InferProto2 adds wait_ms and an optional queue. A server that has no free
// slot answers with one {status: 202, position, eta_ms} line per check and
// keeps the stream open; when a slot is free it sends {status: 200} and the
// body follows. A {status: 202} line with no position says the job holds a
// slot and is waiting on the model server. Old clients never send wait_ms, so
// they never see a 202.
const maxRequest = 16 << 20

// readTimeout bounds how long a peer may take to send its request line.
const readTimeout = 30 * time.Second

type inferReq struct {
	Job  string          `json:"job"` // client-chosen id, named in the receipt
	Path string          `json:"path"`
	Body json.RawMessage `json:"body"`
	Wait int64           `json:"wait_ms,omitempty"` // 1.1.0: how long the server may queue it
}

type inferHdr struct {
	Status   int    `json:"status"`
	Error    string `json:"error,omitempty"`
	Position int    `json:"position,omitempty"` // 202: waiters ahead, 1 = next
	ETA      int64  `json:"eta_ms,omitempty"`   // 202: rough wait, 0 when unknown
}

type peerError struct {
	status int
	msg    string
}

func (e *peerError) Error() string { return fmt.Sprintf("%d: %s", e.status, e.msg) }

// inferPaths are the upstream endpoints a peer may call: OpenAI chat and
// completions, and Anthropic messages (Claude Code and other Anthropic clients).
var inferPaths = map[string]bool{"/chat/completions": true, "/completions": true, "/messages": true}

// meter counts tokens in a response to price it: prefill is cheap relative to decode.
type meter struct {
	prompt, completion int64
	chars              int64
	reported           bool
	done               bool      // the stream reached its terminal event
	end                bool      // the end of the last event: metering stops here
	out                hash.Hash // sha256 of the exact response bytes, for the receipt
}

func (m *meter) raw(b []byte) {
	if m.out == nil {
		m.out = sha256.New()
	}
	m.out.Write(b)
}

func (m *meter) sum() string {
	if m.out == nil {
		m.out = sha256.New()
	}
	return hex.EncodeToString(m.out.Sum(nil))
}

// usage holds token counts in either OpenAI or Anthropic shape.
type usage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
}

func (m *meter) usage(u *usage) {
	if u == nil {
		return
	}
	if u.Prompt > 0 || u.Completion > 0 {
		m.prompt, m.completion = u.Prompt, u.Completion
	}
	// Anthropic streams input tokens in message_start and the running output
	// count in message_delta, so take each when present.
	if u.Input > 0 {
		m.prompt = u.Input
	}
	if u.Output > 0 {
		m.completion = u.Output
	}
	m.reported = true
}

type usageDoc struct {
	Type    string `json:"type"` // Anthropic message_stop
	Usage   *usage `json:"usage"`
	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message"` // Anthropic message_start
	Delta struct {
		Text       string  `json:"text"`
		StopReason *string `json:"stop_reason"`
	} `json:"delta"` // Anthropic content_block_delta
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (m *meter) sseLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		m.done, m.end = true, true
		return
	}
	var d usageDoc
	if json.Unmarshal(data, &d) != nil {
		return
	}
	if d.Type == "message_stop" {
		m.done, m.end = true, true
	}
	if d.Delta.StopReason != nil && *d.Delta.StopReason != "" {
		m.done = true
	}
	m.usage(d.Usage)
	if d.Message != nil {
		m.usage(d.Message.Usage)
	}
	m.chars += int64(len(d.Delta.Text))
	for _, c := range d.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			m.done = true
		}
		m.chars += int64(len(c.Delta.Content))
	}
}

// body meters a whole non-streamed response. One that parses is complete.
func (m *meter) body(b []byte) {
	var d usageDoc
	if json.Unmarshal(b, &d) == nil {
		m.usage(d.Usage)
	}
	m.done = json.Valid(b)
}

// tokens returns the counts to bill. An upstream that ignored include_usage
// is estimated at four characters per completion token.
func (m *meter) tokens() (prompt, completion int64) {
	if !m.reported {
		return 0, m.chars / 4
	}
	return m.prompt, m.completion
}

func (m *meter) credits() int64 { return receipts.Price(m.tokens()) }

// line meters one SSE line. Nothing after the last event counts: the client
// and the server may each read a different amount past it (whichever one the
// other's reset or the local client's hang-up reaches first), and the receipt
// they sign has to match, so both stop at the same place.
func (m *meter) line(b []byte) {
	if m.end {
		return
	}
	m.sseLine(b) // this line may be the end
	m.raw(b)
}

// pipe copies a response body to dst, line by line for SSE so each event is
// flushed as it arrives, and meters it on the way.
func pipe(dst io.Writer, flush func(), src io.Reader, stream bool, m *meter) error {
	if !stream {
		b, err := io.ReadAll(src)
		if err != nil {
			return err
		}
		m.body(b)
		m.raw(b)
		_, err = dst.Write(b)
		return err
	}
	return pipeLines(dst, flush, src, m, 0)
}

// pipeLines copies SSE lines of at most limit bytes (0: any length), flushing
// each one. A line over the limit is an error, and nothing of it is copied.
func pipeLines(dst io.Writer, flush func(), src io.Reader, m *meter, limit int) error {
	r := bufio.NewReaderSize(src, 64<<10)
	var buf []byte
	for {
		line, err := readLine(r, buf, limit)
		buf = line
		if len(line) > 0 {
			m.line(line)
			if _, werr := dst.Write(line); werr != nil {
				return werr
			}
			if flush != nil {
				flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// pipePeer is pipe for an answer from a peer: the size limits apply, and a
// non-streamed answer is written only once it is whole and parses.
func pipePeer(dst io.Writer, flush func(), src io.Reader, stream bool, m *meter) error {
	if stream {
		return pipeLines(dst, flush, src, m, maxPeerLine)
	}
	b, err := io.ReadAll(io.LimitReader(src, maxPeerBody+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > maxPeerBody {
		return errPeerTooLarge
	}
	m.body(b)
	m.raw(b)
	if !m.done {
		return errPeerNotJSON
	}
	_, err = dst.Write(b)
	return err
}

// callUpstream sends an OpenAI or Anthropic request for a public model name to our own server.
func (n *Node) callUpstream(ctx context.Context, path string, raw []byte) (*http.Response, bool, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false, &peerError{400, "bad json"}
	}
	model, _ := body["model"].(string)
	up, ok := n.upstreamModel(model)
	if !ok {
		return nil, false, &peerError{404, fmt.Sprintf("model %q not served here", model)}
	}
	body["model"] = up
	if defuses(path) {
		defuseRequest(body)
	}
	n.clamp(path, body)
	if lim, ok := n.imageLimit(up); ok {
		if d := trimImages(body, lim); d > 0 {
			log.Printf("dropped %d older image(s) from a %s request; the server takes %d", d, model, lim)
		}
	}
	stream, _ := body["stream"].(bool)
	if stream && path != "/messages" { // Anthropic streams always carry usage
		so, _ := body["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		body["stream_options"] = so
	}
	resp, err := n.postUpstream(ctx, path, body)
	// A server that takes fewer images than the request carries says how
	// many it takes. Remember that and send the newest ones only, now and
	// from then on, rather than refuse every later turn of the session.
	if pe, ok := err.(*peerError); ok && pe.status == 400 {
		if lim, ok := imageLimitFrom(pe.msg); ok && trimImages(body, lim) > 0 {
			n.setImageLimit(up, lim)
			log.Printf("%s takes at most %d image(s) per request; sending only the newest %d", model, lim, lim)
			resp, err = n.postUpstream(ctx, path, body)
		}
	}
	if err != nil {
		return nil, false, err
	}
	return resp, stream, nil
}

// postUpstream sends a request to our own server. Any answer but 200 comes
// back as a *peerError carrying the server's status and message.
func (n *Node) postUpstream(ctx context.Context, path string, body map[string]any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", n.cfg.Upstream+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if path == "/messages" {
		// Anthropic-style servers (NVIDIA Flare, Anthropic's own) refuse a
		// /messages request without it; vLLM and SGLang ignore it.
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	n.upstreamAuth(req)
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, &peerError{502, "upstream: " + err.Error()}
	}
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &peerError{resp.StatusCode, string(msg)}
	}
	return resp, nil
}

// specialOpen starts every DeepSeek chat-template token (<｜User｜>,
// <｜deepseek_image｜>, ...). Its bar is the full-width U+FF5C.
const specialOpen = "<｜"

// defuses reports whether requests on path get defused. Legacy /completions
// is left alone: a raw prompt may spell its template tokens on purpose.
func defuses(path string) bool { return path != "/completions" }

// defuse puts a zero-width space after the '<' of every special token in the
// strings of a decoded chat request, so the tokenizer reads them as plain text.
// vLLM refuses a request whose text holds <｜deepseek_image｜>, and a model can
// echo such a token into the history an agent resends on every turn.
func defuse(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		if strings.Contains(x, specialOpen) {
			return strings.ReplaceAll(x, specialOpen, "<\u200b｜"), true
		}
	case map[string]any:
		changed := false
		for k, e := range x {
			if d, ok := defuse(e); ok {
				x[k], changed = d, true
			}
		}
		return x, changed
	case []any:
		changed := false
		for i, e := range x {
			if d, ok := defuse(e); ok {
				x[i], changed = d, true
			}
		}
		return x, changed
	}
	return v, false
}

// keepKeys are request fields passed on as sent: a stop sequence is matched
// against the model's output, where special tokens are not defused.
var keepKeys = map[string]bool{"stop": true, "stop_sequences": true}

// defuseRequest is defuse for a decoded request, except its keepKeys. Only
// top-level keys are kept: a tool argument named stop is still prompt text.
func defuseRequest(body map[string]any) bool {
	changed := false
	for k, e := range body {
		if keepKeys[k] {
			continue
		}
		if d, ok := defuse(e); ok {
			body[k], changed = d, true
		}
	}
	return changed
}

// defuseBody is defuseRequest for a raw request body. A body without special
// tokens comes back unchanged, byte for byte, and without being decoded.
func defuseBody(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("｜")) && !bytes.Contains(raw, []byte(`\uff5c`)) &&
		!bytes.Contains(raw, []byte(`\uFF5C`)) {
		return raw
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var body map[string]any
	if d.Decode(&body) != nil || !defuseRequest(body) {
		return raw
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false) // keep < > & as they are, so the body does not grow
	if e.Encode(body) != nil {
		return raw
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// clamp keeps a peer from asking for more than one bounded answer: one
// choice, and at most MaxTokens new tokens (set when the client gave none,
// except on legacy /completions whose servers default to a short answer).
func (n *Node) clamp(path string, body map[string]any) {
	delete(body, "n")
	delete(body, "best_of")
	if n.cfg.MaxTokens <= 0 {
		return
	}
	limit := float64(n.cfg.MaxTokens)
	set := false
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := body[k].(float64); ok {
			body[k] = min(v, limit)
			set = true
		} else {
			delete(body, k)
		}
	}
	if !set && path != "/completions" {
		body["max_tokens"] = limit
	}
}

// handleInfer serves one inference request from a peer.
func (n *Node) handleInfer(s network.Stream) {
	defer s.Close()
	remote := s.Conn().RemotePeer()
	reply := func(status int, msg string) {
		b, _ := json.Marshal(inferHdr{Status: status, Error: msg})
		s.Write(append(b, '\n'))
	}
	s.SetReadDeadline(time.Now().Add(readTimeout))
	line, err := bufio.NewReader(io.LimitReader(s, maxRequest)).ReadBytes('\n')
	s.SetReadDeadline(time.Time{})
	var req inferReq
	if err != nil || json.Unmarshal(line, &req) != nil || !inferPaths[req.Path] {
		reply(400, "bad request")
		return
	}
	// What the request may cost at most, reserved against the daily quota
	// while it runs. clamp() in callUpstream applies the same bound.
	var meta struct {
		Model               string `json:"model"`
		MaxTokens           int64  `json:"max_tokens"`
		MaxCompletionTokens int64  `json:"max_completion_tokens"`
	}
	json.Unmarshal(req.Body, &meta)
	worst := max(meta.MaxTokens, meta.MaxCompletionTokens)
	if worst <= 0 {
		worst = int64(n.cfg.MaxTokens)
	}
	if n.cfg.MaxTokens > 0 {
		worst = min(worst, int64(n.cfg.MaxTokens)) // clamp() applies the same cap
	}
	// A job also pays for its prompt (a quarter credit per token), so leave
	// room for it. The body is at most maxRequest bytes, so this is bounded.
	amount := receipts.Price(int64(len(req.Body))/4, worst)
	wait := time.Duration(0)
	if s.Protocol() == protocol.InferProto2 {
		ms := min(req.Wait, int64(maxWait/time.Millisecond)) // clamp first: no overflow
		if ms < 0 {
			ms = 0
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	status, msg, a := n.admit(remote, meta.Model, time.Now(), wait, amount)
	if a.w == nil && status != 0 {
		reply(status, msg)
		return
	}
	tk, held := a.t, a.w == nil
	var m meter
	defer func() {
		if held {
			n.done(tk, m.credits(), time.Now())
		}
	}()
	if a.w != nil {
		// Say where it stands, then wait on this stream for a slot. A line
		// every second also tells us the peer is still there: the write
		// fails once it gave up, and the place goes to someone else.
		say := func() bool {
			pos := n.position(a.w)
			hdr, _ := json.Marshal(inferHdr{Status: 202, Position: pos, ETA: n.eta(pos)})
			_, err := s.Write(append(hdr, '\n'))
			return err == nil
		}
		if !say() {
			if !n.cancelWait(a.w) { // it was granted as the write failed
				n.done(a.w.t, 0, time.Now())
			}
			return
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		waiting := true
		for waiting {
			select {
			case <-a.w.ready:
				waiting = false
			case <-tick.C:
				// A slot may have come free without a done() call here
				// (an operator's own request, say), so check again.
				n.dispatch()
				if !say() {
					if !n.cancelWait(a.w) { // it was granted as the write failed
						n.done(a.w.t, 0, time.Now())
					}
					return
				}
			case <-timer.C:
				if n.cancelWait(a.w) {
					reply(429, msgBusy)
					return
				}
				<-a.w.ready // lost the race: the slot is ours
				waiting = false
			}
		}
		tk, held = a.w.t, true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // a failed write to the peer stops the upstream generation too
	resp, stream, err := n.callUpstream(ctx, req.Path, req.Body)
	if err != nil {
		pe := err.(*peerError)
		reply(pe.status, pe.msg)
		return
	}
	defer resp.Body.Close()
	reply(200, "")
	pipe(s, nil, resp.Body, stream, &m)
	n.ledger.Served(remote, m.credits())
	if req.Job != "" {
		var model struct{ Model string }
		json.Unmarshal(req.Body, &model)
		prompt, completion := m.tokens()
		n.receipts.Served(req.Job, remote, model.Model, prompt, completion, m.sum(), m.credits())
	}
}

// infer runs a request on peer p and writes the response to w. started reports
// whether anything reached w; until then the caller may retry another peer.
// A nil error means a complete response, and only that earns a receipt.
// wait is how long the peer may queue it; a peer that predates the queue
// answers at once, as before.
func (n *Node) infer(ctx context.Context, p peer.ID, path string, body []byte, stream bool,
	w http.ResponseWriter, wait time.Duration) (started bool, err error) {
	n.addRelayPaths(p)
	proto := p2pproto.ID(protocol.InferProto2)
	req := inferReq{Job: rand.Text(), Path: path, Body: body}
	if wait > 0 {
		req.Wait = wait.Milliseconds()
	}
	s, err := n.openInfer(ctx, p, proto)
	if errors.Is(err, multistream.ErrNotSupported[p2pproto.ID]{}) {
		// An older node has no queue: ask it the old way. Any other error says
		// nothing about its version, and it goes straight back.
		proto, req.Wait = protocol.InferProto, 0
		s, err = n.openInfer(ctx, p, proto)
	}
	if err != nil {
		return false, err
	}
	// A refusal or a whole answer closes the stream. Anything else resets it,
	// so the peer stops sending.
	defer func() {
		var pe *peerError
		if err != nil && !errors.As(err, &pe) {
			s.Reset()
			return
		}
		s.Close()
	}()
	stop := context.AfterFunc(ctx, func() { s.Reset() })
	defer stop()

	// A peer that stops reading must not hold the request here.
	s.SetWriteDeadline(time.Now().Add(requestWriteStall))
	if _, err := s.Write(requestLine(req)); err != nil {
		return false, err
	}
	s.CloseWrite()
	// Bound the wait for a header: a peer that keeps sending 202 lines must
	// not hold this request (and the gateway's writer) forever. Once the
	// header is in, the deadline restarts with each read, so a long generation
	// is not cut and a silent peer ends.
	grace := headerGrace
	if stream {
		grace = streamHeaderGrace
	}
	deadline := time.Now().Add(wait + grace)
	s.SetReadDeadline(deadline)
	in := &stallReader{s: s}
	r := bufio.NewReaderSize(in, 64<<10)
	var hdr inferHdr
	var firstBeat time.Time
	for {
		// ReadSlice keeps one header line inside the buffer, so a peer
		// cannot make us grow it without bound.
		line, err := r.ReadSlice('\n')
		if err != nil {
			return false, err
		}
		hdr = inferHdr{} // a field this line leaves out must not keep the last line's value
		if err := json.Unmarshal(line, &hdr); err != nil {
			return false, err
		}
		if hdr.Status != 202 {
			break
		}
		if hdr.Position != 0 { // queued; the slot comes on this stream
			continue
		}
		// Position 0: the job holds a slot and waits on its model server.
		now := time.Now()
		if firstBeat.IsZero() {
			firstBeat = now
		}
		next := now.Add(progressStall)
		if last := firstBeat.Add(maxGenerate); next.After(last) {
			next = last
		}
		if next.After(deadline) {
			deadline = next
			s.SetReadDeadline(deadline)
		}
	}
	if hdr.Status != 200 {
		return false, &peerError{hdr.Status, cleanPeerText(hdr.Error, maxPeerText)}
	}
	in.stall = peerStall
	var m meter
	head := &firstByte{w: w, stream: stream, served: p}
	err = pipePeer(head, head.flush, r, stream, &m)
	started = head.flusher != nil
	switch {
	case m.done:
		// The response reached its last event. A client that hangs up here
		// (agents close the request once they see [DONE]) resets the stream
		// while we wait for EOF; the peer did not fail.
		err = nil
	case head.werr != nil:
		err = errLocalGone
	case err == nil && started:
		err = errPeerCut
	case err == nil:
		err = errPeerNoAnswer
	}
	if started {
		n.ledger.Received(p, m.credits())
	}
	if err == nil {
		var model struct{ Model string }
		json.Unmarshal(body, &model)
		go func() {
			if err := n.sendReceipt(p, req.Job, model.Model, m); err != nil {
				log.Printf("receipt to %s: %v", p, err)
			}
		}()
	}
	return started, err
}

// requestLine is the line that starts an inference stream. HTML escaping stays
// off so the body goes out no larger than it came: < would take six bytes, and
// a body within maxGatewayBody could then pass the peer's limit.
func requestLine(req inferReq) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(req) // ends the line
	return b.Bytes()
}

// openInfer opens one inference stream to p. The time limit covers the opening
// only: the stream outlives this context.
func (n *Node) openInfer(ctx context.Context, p peer.ID, proto p2pproto.ID) (network.Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return n.host.NewStream(network.WithAllowLimitedConn(ctx, "infer"), p, proto)
}

// firstByte writes the response head on the first body byte. Until then
// nothing has reached the local client, so the gateway can still try another
// peer. It remembers a failed write: that is the local client gone, not the
// peer failing.
type firstByte struct {
	w       http.ResponseWriter
	stream  bool
	served  peer.ID
	flusher func() // set once the head is written
	werr    error
}

func (h *firstByte) Write(b []byte) (int, error) {
	if h.flusher == nil {
		h.flusher = writeHead(h.w, h.stream, h.served)
	}
	n, err := h.w.Write(b)
	if err != nil {
		h.werr = err
	}
	return n, err
}

func (h *firstByte) flush() {
	if h.flusher != nil {
		h.flusher()
	}
}

// writeHead sets response headers and returns a flusher.
func writeHead(w http.ResponseWriter, stream bool, served peer.ID) func() {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if served != "" {
		w.Header().Set("X-Bitllm-Peer", served.String())
	}
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	return func() {
		if f != nil {
			f.Flush()
		}
	}
}
