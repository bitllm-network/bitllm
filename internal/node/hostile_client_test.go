package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	p2pproto "github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multistream"
)

// The rest of the package's tests ask for models nobody serves right after a
// start, and expect the answer at once. TestStartupWait turns the wait back on.
func init() { startupWait = 0 }

// scriptedPeer is a real libp2p host that answers inference streams from a
// script instead of a model server, and counts what reaches it.
type scriptedPeer struct {
	id       peer.ID
	streams  atomic.Int32 // inference streams opened to it
	receipts atomic.Int32 // receipts delivered to it
}

// script runs for one inference stream, after its request line was read.
type script func(s network.Stream, req inferReq)

// addScriptedPeer starts a scripted peer and makes it a candidate for model on
// client, announcing free slots. It speaks 1.1.0 unless told otherwise.
func addScriptedPeer(t *testing.T, client *Node, model string, free int, run script, protos ...p2pproto.ID) *scriptedPeer {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	sp := &scriptedPeer{id: h.ID()}
	handle := func(s network.Stream) {
		defer s.Close()
		sp.streams.Add(1)
		line, err := bufio.NewReader(s).ReadBytes('\n')
		if err != nil {
			return
		}
		var req inferReq
		if json.Unmarshal(line, &req) != nil {
			return
		}
		run(s, req)
	}
	if len(protos) == 0 {
		protos = []p2pproto.ID{protocol.InferProto2}
	}
	for _, p := range protos {
		h.SetStreamHandler(p, handle)
	}
	h.SetStreamHandler(protocol.ReceiptProto, func(s network.Stream) {
		defer s.Close()
		io.Copy(io.Discard, s)
		sp.receipts.Add(1)
		s.Write([]byte("ok\n"))
	})
	client.host.Peerstore().AddAddrs(h.ID(), h.Addrs(), peerstore.PermanentAddrTTL)
	sp.announce(client, model, free)
	return sp
}

func (sp *scriptedPeer) announce(client *Node, model string, free int) {
	client.remember(sp.id, protocol.Announce{Version: "test", Models: []string{model}, Free: free, Max: 8})
}

// sayHdr writes one header line of the inference protocol.
func sayHdr(s network.Stream, h inferHdr) error {
	b, _ := json.Marshal(h)
	_, err := s.Write(append(b, '\n'))
	return err
}

const (
	answerJSON = `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`
	eventDelta = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	eventDone  = "data: [DONE]\n\n"
)

// honestAnswer answers like a working server, in the form the request asked for.
func honestAnswer(s network.Stream, req inferReq) {
	sayHdr(s, inferHdr{Status: 200})
	var body struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(req.Body, &body)
	if body.Stream {
		io.WriteString(s, eventDelta+eventDone)
		return
	}
	io.WriteString(s, answerJSON)
}

// refuseWith answers every request with one status and message.
func refuseWith(status int, msg string) script {
	return func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: status, Error: msg}) }
}

// holdOpen keeps a stream open until the other side ends it.
func holdOpen(s network.Stream) { io.Copy(io.Discard, s) }

// clientNode is a node with no upstream, on a private network of its own.
func clientNode(t *testing.T) *Node {
	t.Helper()
	return testNode(t, t.Context(), Config{})
}

// gatewayOf serves n's gateway over real HTTP, as a local client reaches it.
func gatewayOf(t *testing.T, n *Node) *httptest.Server {
	t.Helper()
	gw := httptest.NewServer(n.Gateway())
	t.Cleanup(gw.Close)
	return gw
}

// ask sends one request and returns its status, its headers and its whole
// body. readErr is what reading the body ended with.
func ask(t *testing.T, gw *httptest.Server, path, body string) (status int, hdr http.Header, out string, readErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b), readErr
}

const (
	chatPath    = "/v1/chat/completions"
	chatBody    = `{"model":"m","messages":[]}`
	chatStream  = `{"model":"m","stream":true,"messages":[]}`
	messagePath = "/v1/messages"
)

// captureLogs collects log output for the rest of the test.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return logs
}

// setVar changes a package variable for one test.
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// errorMessage reads the message out of a gateway error body of either shape.
func errorMessage(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return doc.Error.Message
}

func TestCleanPeerText(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain", "busy: try later", 100, "busy: try later"},
		{"escape sequences", "\x1b[31mred\x1b[0m\x07", 100, "[31mred[0m"},
		{"line breaks", "one\r\ntwo\tthree", 100, "one  two three"},
		{"invalid utf-8", "a\xff\xfeb", 100, "ab"},
		{"c1 and format controls", "a\u0085b‮c d", 100, "abcd"},
		{"other languages stay", "服务器繁忙 busy", 100, "服务器繁忙 busy"},
		{"cut to max", "abcdef", 3, "abc"},
		{"cut between characters", "服务器", 4, "服"},
		{"nothing allowed", "abc", 0, ""},
	}
	for _, tc := range cases {
		got := cleanPeerText(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if !utf8.ValidString(got) || len(got) > max(tc.max, 0) {
			t.Errorf("%s: %q is not valid or over %d bytes", tc.name, got, tc.max)
		}
	}
	// The refusals the gateway decides on survive the cleaning whole.
	for _, msg := range []string{msgBusy, msgQueueFull, msgUninvited, msgNotAllowed + " 12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib"} {
		got := cleanPeerText(msg, maxPeerText)
		if got != msg {
			t.Errorf("%q changed to %q", msg, got)
		}
	}
	if !isBusy(429, cleanPeerText("busy\x1b[0m\n", maxPeerText)) {
		t.Error("isBusy no longer holds on cleaned text")
	}
	if !notAllowed(403, cleanPeerText(msgNotAllowed+"\x1b[0m", maxPeerText)) {
		t.Error("notAllowed no longer holds on cleaned text")
	}
}

func TestPeerStatus(t *testing.T) {
	for _, s := range []int{400, 403, 404, 413, 422, 429, 500, 502, 503, 504} {
		if got := peerStatus(s); got != s {
			t.Errorf("peerStatus(%d) = %d", s, got)
		}
	}
	for _, s := range []int{-1, 0, 1, 99, 100, 200, 201, 204, 301, 401, 402, 418, 501, 505, 599, 600, 999, 1000} {
		if got := peerStatus(s); got != 502 {
			t.Errorf("peerStatus(%d) = %d, want 502", s, got)
		}
	}
}

// A status no HTTP response can carry reaches the local client as a 502, not
// as a dropped connection.
func TestPeerStatusOutOfRange(t *testing.T) {
	for _, status := range []int{0, 1000, 201} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := clientNode(t)
			gw := gatewayOf(t, client)
			addScriptedPeer(t, client, "m", 1, refuseWith(status, "no"))
			got, _, body, err := ask(t, gw, chatPath, chatBody)
			if err != nil || got != 502 {
				t.Fatalf("got %d %q, %v; want 502", got, body, err)
			}
		})
	}
}

// A refusal keeps its status when it is one the client can act on.
func TestPeerStatusPassedOn(t *testing.T) {
	client := clientNode(t)
	gw := gatewayOf(t, client)
	addScriptedPeer(t, client, "m", 1, refuseWith(404, "no such model"))
	got, _, body, _ := ask(t, gw, chatPath, chatBody)
	if got != 404 || errorMessage(t, body) != "all peers refused: no such model" {
		t.Fatalf("got %d %q", got, body)
	}
}

// Terminal control sequences in a peer's error reach neither the log nor the
// local client.
func TestPeerErrorCleaned(t *testing.T) {
	client := clientNode(t)
	gw := gatewayOf(t, client)
	logs := captureLogs(t)
	addScriptedPeer(t, client, "m", 1, refuseWith(500, "bad\x1b]0;title\x07\ninjected line\xff"+strings.Repeat("x", 2*maxPeerText)))
	got, _, body, _ := ask(t, gw, chatPath, chatBody)
	if got != 500 {
		t.Fatalf("got %d %q", got, body)
	}
	msg := errorMessage(t, body)
	if strings.ContainsAny(msg, "\x1b\x07\n\xff") || !strings.Contains(msg, "bad]0;title injected line") {
		t.Fatalf("message not cleaned: %q", msg)
	}
	if len(msg) > maxPeerText+len("all peers refused: ") {
		t.Fatalf("message not capped: %d bytes", len(msg))
	}
	out := logs.String()
	if !strings.Contains(out, "injected line") {
		t.Fatalf("the failure was not logged: %q", out)
	}
	if strings.ContainsAny(out, "\x1b\x07") || strings.Contains(out, "\ninjected line") {
		t.Fatalf("log line not cleaned: %q", out)
	}
}

// withBackup makes a first candidate that runs the script, and a backup that
// answers if the gateway moves on from it.
func withBackup(t *testing.T, run script) (gw *httptest.Server, first, backup *scriptedPeer) {
	t.Helper()
	client := clientNode(t)
	gw = gatewayOf(t, client)
	first = addScriptedPeer(t, client, "m", 4, run)
	backup = addScriptedPeer(t, client, "m", 0, honestAnswer)
	return gw, first, backup
}

// Each of these leaves nothing written yet, so the next peer answers and the
// client never hears of the failure.
func TestFailoverBeforeFirstByte(t *testing.T) {
	endless := func(s network.Stream) {
		chunk := []byte(strings.Repeat("a", 4096))
		for {
			if _, err := s.Write(chunk); err != nil {
				return
			}
		}
	}
	cases := []struct {
		name string
		body string
		run  script
	}{
		{"200 then nothing", chatBody, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}) }},
		{"200 then nothing, streamed", chatStream, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}) }},
		{"200 then silence", chatBody, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}); holdOpen(s) }},
		{"200 then silence, streamed", chatStream, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}); holdOpen(s) }},
		{"200 then a reset", chatBody, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}); s.Reset() }},
		{"a body that stops halfway", chatBody, func(s network.Stream, _ inferReq) {
			sayHdr(s, inferHdr{Status: 200})
			io.WriteString(s, answerJSON[:10])
		}},
		{"an oversized body", chatBody, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}); endless(s) }},
		{"an endless line", chatStream, func(s network.Stream, _ inferReq) { sayHdr(s, inferHdr{Status: 200}); endless(s) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setVar(t, &peerStall, 300*time.Millisecond)
			setVar(t, &maxPeerBody, int64(64<<10))
			setVar(t, &maxPeerLine, 64<<10)
			gw, first, backup := withBackup(t, tc.run)
			got, hdr, body, err := ask(t, gw, chatPath, tc.body)
			if err != nil || got != 200 {
				t.Fatalf("got %d %q, %v", got, body, err)
			}
			if first.streams.Load() != 1 || hdr.Get("X-Bitllm-Peer") != backup.id.String() {
				t.Fatalf("answered by %s, first peer saw %d streams", hdr.Get("X-Bitllm-Peer"), first.streams.Load())
			}
			eventually(t, 5*time.Second, "the peer that answered got no receipt", func() bool { return backup.receipts.Load() == 1 })
			if first.receipts.Load() != 0 {
				t.Fatal("a peer that sent no whole answer got a receipt")
			}
		})
	}
}

// A size limit also resets the stream, which stops the peer sending.
func TestOverLimitResetsStream(t *testing.T) {
	for name, body := range map[string]string{"body": chatBody, "line": chatStream} {
		t.Run(name, func(t *testing.T) {
			setVar(t, &maxPeerBody, int64(64<<10))
			setVar(t, &maxPeerLine, 64<<10)
			stopped := make(chan error, 1)
			client := clientNode(t)
			gw := gatewayOf(t, client)
			addScriptedPeer(t, client, "m", 1, func(s network.Stream, _ inferReq) {
				sayHdr(s, inferHdr{Status: 200})
				chunk := []byte(strings.Repeat("a", 4096))
				for {
					if _, err := s.Write(chunk); err != nil {
						stopped <- err
						return
					}
				}
			})
			got, _, out, err := ask(t, gw, chatPath, body)
			if err != nil || got != 502 {
				t.Fatalf("got %d %q, %v", got, out, err)
			}
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("the peer could keep writing past the limit")
			}
		})
	}
}

// A slow answer that keeps moving is not a stall: the deadline restarts with
// each read.
func TestSlowAnswerSurvivesStall(t *testing.T) {
	setVar(t, &peerStall, 500*time.Millisecond)
	client := clientNode(t)
	gw := gatewayOf(t, client)
	addScriptedPeer(t, client, "m", 1, func(s network.Stream, _ inferReq) {
		sayHdr(s, inferHdr{Status: 200})
		for range 6 {
			io.WriteString(s, eventDelta)
			time.Sleep(200 * time.Millisecond)
		}
		io.WriteString(s, eventDone)
	})
	got, _, body, err := ask(t, gw, chatPath, chatStream)
	if err != nil || got != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("got %d %q, %v", got, body, err)
	}
}

// Once part of an answer went out, an answer that does not end reaches the
// local client as a broken connection, not as a clean end, and earns no receipt.
func TestCutStreamAborts(t *testing.T) {
	cases := []struct {
		name string
		run  script
	}{
		{"clean close", func(s network.Stream, _ inferReq) {
			sayHdr(s, inferHdr{Status: 200})
			io.WriteString(s, eventDelta)
		}},
		{"reset", func(s network.Stream, _ inferReq) {
			sayHdr(s, inferHdr{Status: 200})
			io.WriteString(s, eventDelta)
			time.Sleep(100 * time.Millisecond)
			s.Reset()
		}},
		{"silence", func(s network.Stream, _ inferReq) {
			sayHdr(s, inferHdr{Status: 200})
			io.WriteString(s, eventDelta)
			holdOpen(s)
		}},
		{"endless line", func(s network.Stream, _ inferReq) {
			sayHdr(s, inferHdr{Status: 200})
			io.WriteString(s, eventDelta)
			chunk := []byte(strings.Repeat("a", 4096))
			for {
				if _, err := s.Write(chunk); err != nil {
					return
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setVar(t, &peerStall, 300*time.Millisecond)
			setVar(t, &maxPeerLine, 64<<10)
			logs := captureLogs(t)
			gw, first, backup := withBackup(t, tc.run)
			got, _, body, err := ask(t, gw, chatPath, chatStream)
			if got != 200 || !strings.Contains(body, `"hi"`) {
				t.Fatalf("got %d %q", got, body)
			}
			if err == nil {
				t.Fatalf("the client saw a clean end of a cut answer: %q", body)
			}
			if !strings.Contains(logs.String(), "broke mid-response") {
				t.Fatalf("not logged: %q", logs.String())
			}
			if backup.streams.Load() != 0 {
				t.Fatal("a second peer was asked after bytes had gone out")
			}
			time.Sleep(200 * time.Millisecond)
			if first.receipts.Load() != 0 {
				t.Fatal("a cut answer got a receipt")
			}
		})
	}
}

// A local client that hangs up mid-answer is not the peer's failure.
func TestClientHangUpIsNotPeerFailure(t *testing.T) {
	logs := captureLogs(t)
	client := clientNode(t)
	gw := gatewayOf(t, client)
	sp := addScriptedPeer(t, client, "m", 1, func(s network.Stream, _ inferReq) {
		sayHdr(s, inferHdr{Status: 200})
		io.WriteString(s, eventDelta)
		holdOpen(s)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+chatPath, strings.NewReader(chatStream))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	eventually(t, 5*time.Second, "the request did not end", func() bool { return client.gatewayLive.Load() == 0 })
	if out := logs.String(); strings.Contains(out, "broke mid-response") {
		t.Fatalf("logged as a peer failure: %q", out)
	}
	if cooling, _ := client.peerMem.rank(sp.id, "m", time.Now()); cooling {
		t.Fatal("the peer went into cooldown because the local client left")
	}
}

// Audit canaries call infer with a recorder, so a cut answer must come back as
// an error rather than a panic.
func TestInferCutStreamReturnsError(t *testing.T) {
	client := clientNode(t)
	sp := addScriptedPeer(t, client, "m", 1, func(s network.Stream, _ inferReq) {
		sayHdr(s, inferHdr{Status: 200})
		io.WriteString(s, eventDelta)
	})
	started, err := client.infer(t.Context(), sp.id, "/chat/completions", []byte(chatStream), true, httptest.NewRecorder(), 0)
	if !started || err == nil {
		t.Fatalf("started=%v err=%v", started, err)
	}
}

// A slow failure on the first peer leaves the second its turn, though the time
// allowed for waiting on busy peers has run out.
func TestFailoverAfterBusyWait(t *testing.T) {
	setVar(t, &peerStall, 400*time.Millisecond)
	setVar(t, &busyWait, 100*time.Millisecond)
	gw, _, backup := withBackup(t, func(s network.Stream, _ inferReq) {
		sayHdr(s, inferHdr{Status: 200})
		holdOpen(s)
	})
	got, hdr, body, err := ask(t, gw, chatPath, chatBody)
	if err != nil || got != 200 || hdr.Get("X-Bitllm-Peer") != backup.id.String() {
		t.Fatalf("got %d %q %s, %v", got, body, hdr.Get("X-Bitllm-Peer"), err)
	}
}

// beat sends a header line every 50 ms until the stream ends or until is over.
func beat(s network.Stream, h inferHdr, until time.Duration) {
	for end := time.Now().Add(until); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if sayHdr(s, h) != nil {
			return
		}
	}
}

// inferOnce runs one request against a scripted peer directly and reports how
// it ended and how long that took.
func inferOnce(t *testing.T, stream bool, run script) (started bool, took time.Duration, err error) {
	t.Helper()
	client := clientNode(t)
	sp := addScriptedPeer(t, client, "m", 1, run)
	body := chatBody
	if stream {
		body = chatStream
	}
	start := time.Now()
	started, err = client.infer(t.Context(), sp.id, "/chat/completions", []byte(body), stream, httptest.NewRecorder(), 0)
	return started, time.Since(start), err
}

// Lines saying the job holds a slot keep a long wait for the model server alive.
func TestHeartbeatExtendsHeaderWait(t *testing.T) {
	setVar(t, &headerGrace, 200*time.Millisecond)
	setVar(t, &progressStall, 300*time.Millisecond)
	started, _, err := inferOnce(t, false, func(s network.Stream, req inferReq) {
		beat(s, inferHdr{Status: 202}, time.Second)
		honestAnswer(s, req)
	})
	if !started || err != nil {
		t.Fatalf("started=%v err=%v", started, err)
	}
}

// The first heartbeat often follows queue lines. A line with no position means
// position 0 even after lines that named one.
func TestHeartbeatAfterQueueLines(t *testing.T) {
	setVar(t, &headerGrace, 200*time.Millisecond)
	setVar(t, &progressStall, 300*time.Millisecond)
	started, _, err := inferOnce(t, false, func(s network.Stream, req inferReq) {
		sayHdr(s, inferHdr{Status: 202, Position: 3, ETA: 500})
		beat(s, inferHdr{Status: 202}, time.Second)
		honestAnswer(s, req)
	})
	if !started || err != nil {
		t.Fatalf("started=%v err=%v", started, err)
	}
}

// Queue lines extend nothing: past the queue bound the request ends.
func TestQueueLinesDoNotExtend(t *testing.T) {
	setVar(t, &headerGrace, 300*time.Millisecond)
	setVar(t, &progressStall, time.Hour)
	for _, pos := range []int{1, 7, -1} {
		started, took, err := inferOnce(t, false, func(s network.Stream, _ inferReq) {
			beat(s, inferHdr{Status: 202, Position: pos, ETA: 1000}, 20*time.Second)
		})
		if started || err == nil || took > 3*time.Second {
			t.Fatalf("position %d: started=%v err=%v after %v", pos, started, err, took)
		}
	}
}

// A queue line after a heartbeat does not push the deadline either.
func TestQueueLinesAfterHeartbeatDoNotExtend(t *testing.T) {
	setVar(t, &headerGrace, 100*time.Millisecond)
	setVar(t, &progressStall, 400*time.Millisecond)
	started, took, err := inferOnce(t, false, func(s network.Stream, _ inferReq) {
		sayHdr(s, inferHdr{Status: 202})
		beat(s, inferHdr{Status: 202, Position: 1}, 20*time.Second)
	})
	if started || err == nil || took > 3*time.Second {
		t.Fatalf("started=%v err=%v after %v", started, err, took)
	}
}

// Heartbeats alone cannot hold a request past maxGenerate.
func TestHeartbeatBoundedOverall(t *testing.T) {
	setVar(t, &headerGrace, 100*time.Millisecond)
	setVar(t, &progressStall, 200*time.Millisecond)
	setVar(t, &maxGenerate, 500*time.Millisecond)
	started, took, err := inferOnce(t, false, func(s network.Stream, _ inferReq) {
		beat(s, inferHdr{Status: 202}, 20*time.Second)
	})
	if started || err == nil || took > 3*time.Second {
		t.Fatalf("started=%v err=%v after %v", started, err, took)
	}
}

// A heartbeat that goes quiet ends the request after progressStall.
func TestHeartbeatStops(t *testing.T) {
	setVar(t, &headerGrace, 100*time.Millisecond)
	setVar(t, &progressStall, 300*time.Millisecond)
	started, took, err := inferOnce(t, false, func(s network.Stream, _ inferReq) {
		sayHdr(s, inferHdr{Status: 202})
		holdOpen(s)
	})
	if started || err == nil || took > 3*time.Second {
		t.Fatalf("started=%v err=%v after %v", started, err, took)
	}
}

// A heartbeat never shortens the time the request already had.
func TestHeartbeatKeepsLongerDeadline(t *testing.T) {
	setVar(t, &headerGrace, 800*time.Millisecond)
	setVar(t, &progressStall, 100*time.Millisecond)
	started, _, err := inferOnce(t, false, func(s network.Stream, req inferReq) {
		sayHdr(s, inferHdr{Status: 202})
		time.Sleep(400 * time.Millisecond)
		honestAnswer(s, req)
	})
	if !started || err != nil {
		t.Fatalf("started=%v err=%v", started, err)
	}
}

// A server too old to send heartbeats gets more time before its first line
// when not streaming, since a non-streamed answer sends nothing until it is done.
func TestHeaderGraceForOldServers(t *testing.T) {
	if headerGrace != 180*time.Second || streamHeaderGrace != 60*time.Second {
		t.Fatalf("grace %v and %v, want 180 s and 60 s", headerGrace, streamHeaderGrace)
	}
	setVar(t, &headerGrace, 1500*time.Millisecond)
	setVar(t, &streamHeaderGrace, 200*time.Millisecond)
	late := func(s network.Stream, req inferReq) {
		time.Sleep(600 * time.Millisecond)
		honestAnswer(s, req)
	}
	if started, _, err := inferOnce(t, false, late); !started || err != nil {
		t.Fatalf("non-streamed: started=%v err=%v", started, err)
	}
	if started, _, err := inferOnce(t, true, late); started || err == nil {
		t.Fatalf("streamed: started=%v err=%v, want the shorter grace to end it", started, err)
	}
}

// A node that registers only the old protocol still answers, through the gateway.
func TestOldProtocolPeer(t *testing.T) {
	client := clientNode(t)
	gw := gatewayOf(t, client)
	var wait atomic.Int64
	sp := addScriptedPeer(t, client, "m", 1, func(s network.Stream, req inferReq) {
		wait.Store(req.Wait)
		honestAnswer(s, req)
	}, protocol.InferProto)
	got, hdr, body, err := ask(t, gw, chatPath, chatBody)
	if err != nil || got != 200 || hdr.Get("X-Bitllm-Peer") != sp.id.String() {
		t.Fatalf("got %d %q, %v", got, body, err)
	}
	if wait.Load() != 0 {
		t.Fatalf("the old protocol carried wait_ms=%d", wait.Load())
	}
}

// countingHost counts stream attempts per protocol and fails them with err.
type countingHost struct {
	host.Host
	err    error
	tries  map[p2pproto.ID]int
	bounds []bool // whether each attempt carried a deadline
}

func (h *countingHost) NewStream(ctx context.Context, _ peer.ID, pids ...p2pproto.ID) (network.Stream, error) {
	h.tries[pids[0]]++
	_, ok := ctx.Deadline()
	h.bounds = append(h.bounds, ok)
	return nil, h.err
}

// Only a refusal of the protocol is a reason to try the old one.
func TestOldProtocolOnlyOnNegotiationFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"dial error", errors.New("dial failed"), 0},
		{"deadline", context.DeadlineExceeded, 0},
		{"not supported", fmt.Errorf("failed to negotiate protocol: %w",
			multistream.ErrNotSupported[p2pproto.ID]{Protos: []p2pproto.ID{protocol.InferProto2}}), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := libp2p.New(libp2p.NoListenAddrs)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			fake := &countingHost{Host: h, err: tc.err, tries: map[p2pproto.ID]int{}}
			n := &Node{host: fake, peers: map[peer.ID]*peerInfo{}}
			started, err := n.infer(t.Context(), randomPeer(t), "/chat/completions", []byte(chatBody), false, httptest.NewRecorder(), 0)
			if started || !errors.Is(err, tc.err) {
				t.Fatalf("started=%v err=%v", started, err)
			}
			if fake.tries[protocol.InferProto2] != 1 || fake.tries[protocol.InferProto] != tc.want {
				t.Fatalf("attempts %v, want one on 1.1.0 and %d on 1.0.0", fake.tries, tc.want)
			}
			for i, bounded := range fake.bounds {
				if !bounded {
					t.Fatalf("attempt %d had no deadline", i)
				}
			}
		})
	}
}

// A peer that accepts the connection and then says nothing ends at dialTimeout.
func TestDialBounded(t *testing.T) {
	if dialTimeout != 10*time.Second {
		t.Fatalf("dialTimeout = %v", dialTimeout)
	}
	setVar(t, &dialTimeout, 300*time.Millisecond)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	addr, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", l.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	client := clientNode(t)
	silent := randomPeer(t)
	client.host.Peerstore().AddAddr(silent, addr, peerstore.PermanentAddrTTL)
	start := time.Now()
	started, err := client.infer(t.Context(), silent, "/chat/completions", []byte(chatBody), false, httptest.NewRecorder(), 0)
	if started || err == nil {
		t.Fatalf("started=%v err=%v", started, err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %v with a %v limit", took, dialTimeout)
	}
}

// Three peers refuse the first request and use up its three tries. They cool
// down, and the next request reaches the honest peer behind them.
func TestRefusersCoolDown(t *testing.T) {
	client := clientNode(t)
	gw := gatewayOf(t, client)
	var refusers []*scriptedPeer
	for range 3 {
		refusers = append(refusers, addScriptedPeer(t, client, "m", 8, refuseWith(500, "no")))
	}
	honest := addScriptedPeer(t, client, "m", 0, honestAnswer)

	got, _, body, _ := ask(t, gw, chatPath, chatBody)
	if got != 500 {
		t.Fatalf("first request: %d %q", got, body)
	}
	if honest.streams.Load() != 0 {
		t.Fatal("the honest peer was asked before the three refusers; the setup needs them first")
	}
	for i, r := range refusers {
		if r.streams.Load() != 1 {
			t.Fatalf("refuser %d saw %d streams", i, r.streams.Load())
		}
	}

	got, hdr, body, err := ask(t, gw, chatPath, chatBody)
	if err != nil || got != 200 || hdr.Get("X-Bitllm-Peer") != honest.id.String() {
		t.Fatalf("second request: %d %q %s, %v", got, body, hdr.Get("X-Bitllm-Peer"), err)
	}
	for i, r := range refusers {
		if r.streams.Load() != 1 {
			t.Fatalf("refuser %d was asked again during its cooldown", i)
		}
	}
}

// A sole server in cooldown is still asked, and its answer ends the cooldown.
func TestSoleServerRetried(t *testing.T) {
	client := clientNode(t)
	gw := gatewayOf(t, client)
	var healthy atomic.Bool
	sp := addScriptedPeer(t, client, "m", 1, func(s network.Stream, req inferReq) {
		if !healthy.Load() {
			sayHdr(s, inferHdr{Status: 503, Error: "not yet"})
			return
		}
		honestAnswer(s, req)
	})
	if got, _, body, _ := ask(t, gw, chatPath, chatBody); got != 503 {
		t.Fatalf("got %d %q", got, body)
	}
	if cooling, _ := client.peerMem.rank(sp.id, "m", time.Now()); !cooling {
		t.Fatal("no cooldown after a refusal")
	}
	healthy.Store(true)
	if got, _, body, _ := ask(t, gw, chatPath, chatBody); got != 200 {
		t.Fatalf("got %d %q", got, body)
	}
	if cooling, recent := client.peerMem.rank(sp.id, "m", time.Now()); cooling || !recent {
		t.Fatalf("after an answer: cooling=%v recent=%v", cooling, recent)
	}
}

// A busy refusal is not a failure.
func TestBusyDoesNotCool(t *testing.T) {
	setVar(t, &busyWait, 200*time.Millisecond)
	setVar(t, &busyBackoff, 10*time.Millisecond)
	client := clientNode(t)
	gw := gatewayOf(t, client)
	sp := addScriptedPeer(t, client, "m", 1, refuseWith(429, msgBusy))
	got, hdr, body, _ := ask(t, gw, chatPath, chatBody)
	if got != 429 {
		t.Fatalf("got %d %q %v", got, body, hdr)
	}
	if cooling, _ := client.peerMem.rank(sp.id, "m", time.Now()); cooling {
		t.Fatal("a busy peer went into cooldown")
	}
}

func TestPeerMemoryCooldown(t *testing.T) {
	var pm peerMemory
	p := randomPeer(t)
	now := time.Now()
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, d := range want {
		pm.failed(p, "m", now)
		pm.failed(p, "m", now.Add(time.Second)) // during the cooldown: the same incident
		if cooling, _ := pm.rank(p, "m", now.Add(d-time.Millisecond)); !cooling {
			t.Fatalf("failure %d: over before %v", i, d)
		}
		if cooling, _ := pm.rank(p, "m", now.Add(d)); cooling {
			t.Fatalf("failure %d: still cooling after %v", i, d)
		}
		now = now.Add(d)
	}
	if cooling, _ := pm.rank(p, "other", time.Now()); cooling {
		t.Fatal("a cooldown for one model reached another")
	}
	pm.failed(p, "m", now)
	pm.answered(p, "m", now.Add(time.Second))
	if cooling, recent := pm.rank(p, "m", now.Add(time.Second)); cooling || !recent {
		t.Fatalf("after an answer: cooling=%v recent=%v", cooling, recent)
	}
	pm.failed(p, "m", now.Add(time.Hour))
	if cooling, _ := pm.rank(p, "m", now.Add(time.Hour+time.Minute)); cooling {
		t.Fatal("an answer did not reset the cooldown to its shortest")
	}
}

func TestPeerMemoryCapped(t *testing.T) {
	var pm peerMemory
	first := randomPeer(t)
	now := time.Now()
	pm.failed(first, "m", now)
	for i := range maxPeerMemory + 10 {
		pm.failed(first, fmt.Sprint("model", i), now.Add(time.Duration(i+1)*time.Millisecond))
	}
	if len(pm.m) != maxPeerMemory {
		t.Fatalf("%d entries, want %d", len(pm.m), maxPeerMemory)
	}
	if cooling, _ := pm.rank(first, "m", now.Add(time.Second)); cooling {
		t.Fatal("the oldest entry was not the one evicted")
	}
}

// Ranking: cooldown behind everything, then audit suspects and free slots as
// before, then peers that answered recently, and random order among equals.
func TestCandidateRanking(t *testing.T) {
	ctx := t.Context()
	n := testNode(t, ctx, Config{Audit: true})
	add := func(free int) peer.ID {
		p := randomPeer(t)
		n.remember(p, protocol.Announce{Version: "test", Models: []string{"m"}, Free: free, Max: 8})
		return p
	}
	cooling, suspect := add(8), add(8)
	busyAnswered, busy := add(0), add(0)
	n.peerMem.failed(cooling, "m", time.Now())
	n.audits.peers[auditKey{suspect, "m"}] = &peerAudit{State: "suspect"}
	n.peerMem.answered(busyAnswered, "m", time.Now())
	var free []peer.ID
	for range 8 {
		free = append(free, add(1))
	}
	answered := free[3]
	n.peerMem.answered(answered, "m", time.Now())

	firsts := map[peer.ID]bool{}
	for range 40 {
		got := n.candidates("m")
		if len(got) != 12 {
			t.Fatalf("%d candidates, want 12", len(got))
		}
		if got[0] != answered {
			t.Fatalf("a free peer that answered is not first: %v", got[0])
		}
		firsts[got[1]] = true
		if got[8] != busyAnswered || got[9] != busy {
			t.Fatalf("busy peers out of order: %v", got[8:10])
		}
		if got[10] != suspect || got[11] != cooling {
			t.Fatalf("tail: %v, want the suspect, then the peer in cooldown", got[10:])
		}
	}
	if len(firsts) < 2 {
		t.Fatal("equal peers always come in the same order")
	}
}

// A request right after a start waits for the first announce instead of
// getting a 503.
func TestStartupWait(t *testing.T) {
	if startupWindow != 30*time.Second || startupPoll != 250*time.Millisecond {
		t.Fatalf("window %v, poll %v", startupWindow, startupPoll)
	}
	setVar(t, &startupWait, 15*time.Second)
	setVar(t, &startupPoll, 20*time.Millisecond)
	client := clientNode(t)
	gw := gatewayOf(t, client)
	time.AfterFunc(300*time.Millisecond, func() { addScriptedPeer(t, client, "m", 1, honestAnswer) })
	got, _, body, err := ask(t, gw, chatPath, chatBody)
	if err != nil || got != 200 {
		t.Fatalf("got %d %q, %v", got, body, err)
	}
}

func TestStartupWaitEnds(t *testing.T) {
	setVar(t, &startupPoll, 20*time.Millisecond)
	t.Run("runs out", func(t *testing.T) {
		setVar(t, &startupWait, 200*time.Millisecond)
		gw := gatewayOf(t, clientNode(t))
		start := time.Now()
		got, _, body, _ := ask(t, gw, chatPath, chatBody)
		if got != 503 || time.Since(start) < 200*time.Millisecond {
			t.Fatalf("got %d %q after %v", got, body, time.Since(start))
		}
	})
	t.Run("not long after a start", func(t *testing.T) {
		setVar(t, &startupWait, time.Minute)
		setVar(t, &startupWindow, 0)
		gw := gatewayOf(t, clientNode(t))
		if got, _, body, _ := ask(t, gw, chatPath, chatBody); got != 503 {
			t.Fatalf("got %d %q", got, body)
		}
	})
	t.Run("request goes away", func(t *testing.T) {
		setVar(t, &startupWait, time.Minute)
		client := clientNode(t)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		if c := client.awaitCandidates(ctx, "m"); c != nil {
			t.Fatalf("candidates %v", c)
		}
		if ctx.Err() == nil {
			t.Fatal("returned before the request went away")
		}
	})
}
