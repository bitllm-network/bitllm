package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
)

// slowFirstByte sends headers at once and its first body byte after delay.
func slowFirstByte(t *testing.T, delay time.Duration, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(delay):
			fmt.Fprint(w, body)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func streamReq(wait time.Duration, progress bool) inferReq {
	return inferReq{Path: "/chat/completions", Wait: wait.Milliseconds(), Progress: progress,
		Body: json.RawMessage(`{"model":"ds","stream":true,"messages":[{"role":"user","content":"hi"}]}`)}
}

const doneSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// A long prompt can keep the first byte minutes away. That is not a stall: the
// job goes on, the client hears progress meanwhile, and the server stays up.
func TestLongPrefillIsNotAStall(t *testing.T) {
	setVar(t, &upstreamStall, 150*time.Millisecond)
	setVar(t, &progressEvery, 50*time.Millisecond)
	n := servedBy(t, slowFirstByte(t, 600*time.Millisecond, doneSSE), Config{Concurrency: 1})
	s := openInfer(t, rawPeer(t, n, nil, ""), n, protocol.InferProto2, streamReq(time.Second, true), true)
	br := bufio.NewReader(s)
	beats := 0
	for {
		_, h := readHdr(t, br, s, 5*time.Second)
		if h.Status == 200 {
			break
		}
		if h.Status != 202 || h.Position != 0 {
			t.Fatalf("header %+v", h)
		}
		beats++
	}
	if beats < 3 {
		t.Fatalf("only %d progress lines while the prompt was read", beats)
	}
	body, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("body %q, %v", body, err)
	}
	if !n.upOK.Load() {
		t.Fatal("a slow first byte marked our server down")
	}
}

// Once it has started, a server that goes quiet still counts as stalled.
func TestStallAfterFirstByte(t *testing.T) {
	var fired, cancelled bool
	pr, pw := io.Pipe()
	defer pw.Close()
	b := &stallBody{ReadCloser: pr, first: time.Hour, wait: 30 * time.Millisecond,
		cancel: func() { cancelled = true }, fired: func() { fired = true; pr.CloseWithError(io.ErrUnexpectedEOF) }}
	go pw.Write([]byte("x"))
	if _, err := b.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("a quiet server was not cut off")
	}
	if !fired || cancelled {
		t.Fatalf("fired=%v cancelled=%v", fired, cancelled)
	}
}

// A client that does not say it reads progress lines keeps one fixed deadline
// for the 200, so it still gets it as soon as our server answers.
func TestOlderClientGets200AtOnce(t *testing.T) {
	setVar(t, &progressEvery, time.Hour)
	n := servedBy(t, slowFirstByte(t, 2*time.Second, doneSSE), Config{Concurrency: 1})
	s := openInfer(t, rawPeer(t, n, nil, ""), n, protocol.InferProto2, streamReq(time.Second, false), true)
	start := time.Now()
	if _, h := readHdr(t, bufio.NewReader(s), s, 5*time.Second); h.Status != 200 {
		t.Fatalf("header %+v", h)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the 200 waited %v for the first byte", took)
	}
}

// A server that says 200 and sends nothing gives an error, not an empty answer.
func TestEmptyAnswerIsAnError(t *testing.T) {
	setVar(t, &progressEvery, time.Hour)
	n := servedBy(t, slowFirstByte(t, 0, ""), Config{Concurrency: 1})
	s := openInfer(t, rawPeer(t, n, nil, ""), n, protocol.InferProto2, streamReq(time.Second, true), true)
	if _, h := readHdr(t, bufio.NewReader(s), s, 5*time.Second); h.Status != 502 || h.Error != msgUpstream {
		t.Fatalf("header %+v", h)
	}
}

// Free ids cannot take every place: the last readers and the last of the byte
// budget stay open for nodes this server knows.
func TestStrangersLeaveRoomForKnownNodes(t *testing.T) {
	var g requestGate
	g.budget = 1000
	for i := range maxReaders - knownReaders {
		if !g.enter(newPeerID(t), false) {
			t.Fatalf("stranger %d refused too early", i)
		}
	}
	if g.enter(newPeerID(t), false) {
		t.Fatal("a stranger took a place kept for known nodes")
	}
	for i := range knownReaders {
		if !g.enter(newPeerID(t), true) {
			t.Fatalf("known node %d refused", i)
		}
	}
	if g.enter(newPeerID(t), true) {
		t.Fatal("over the total")
	}
	stranger := &bodyHold{gate: &g}
	if stranger.resize(751) {
		t.Fatal("a stranger took the budget kept for known nodes")
	}
	if !stranger.resize(750) {
		t.Fatal("a stranger was refused its share")
	}
	known := &bodyHold{gate: &g, known: true}
	if !known.resize(250) {
		t.Fatal("a known node was refused the rest")
	}
	if known.resize(251) {
		t.Fatal("over the budget")
	}
}

// While shutting down, a node takes no new peer jobs.
func TestClosingRefusesNewJobs(t *testing.T) {
	up, _ := prefillUpstream(t, 0)
	n := servedBy(t, up, Config{Concurrency: 1})
	n.closing.Store(true)
	s := openInfer(t, rawPeer(t, n, nil, ""), n, protocol.InferProto2, chatReq(time.Second), true)
	if _, h := readHdr(t, bufio.NewReader(s), s, 5*time.Second); h.Status != 503 {
		t.Fatalf("header %+v", h)
	}
}
