package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	p2pproto "github.com/libp2p/go-libp2p/core/protocol"
)

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
