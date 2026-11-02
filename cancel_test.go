package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer collects logs written by node goroutines during the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An agent closes the HTTP request as soon as it has the last SSE event, while
// the upstream can keep its body open for a moment longer. The resulting local
// stream reset is not a peer failure, and the completed job must still produce
// a receipt.
func TestClientCancelAfterLastEvent(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseNow := func() { once.Do(func() { close(release) }) }

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":10}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			w.(http.Flusher).Flush()
			<-release
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	defer releaseNow()

	ctx := t.Context()
	boot := testNode(t, ctx, Config{})
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1",
		Models: map[string]string{"deepseek-v4.1-flash": "local-ds"}, FreeDaily: 30})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})

	deadline := time.Now().Add(20 * time.Second)
	for len(client.candidates("deepseek-v4.1-flash")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client never discovered the server")
		}
		time.Sleep(100 * time.Millisecond)
	}

	var logs lockedBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	gw := httptest.NewServer(client.Gateway())
	defer gw.Close()

	reqCtx, cancel := context.WithCancel(ctx)
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequestWithContext(reqCtx, "POST", gw.URL+"/v1/chat/completions", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "data: [DONE]" {
			break
		}
	}
	// Done with the response: hang up before the upstream ends.
	cancel()
	resp.Body.Close()
	time.Sleep(300 * time.Millisecond)
	releaseNow()

	time.Sleep(500 * time.Millisecond)
	if strings.Contains(logs.String(), "broke mid-response") {
		t.Fatalf("gateway logged a peer failure after a complete response:\n%s", logs.String())
	}

	srvID := server.host.ID().String()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if c := server.receipts.Tally()[srvID]; c.Jobs > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no receipt after a complete response: %+v", server.receipts.Tally())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestMeterTerminalEvents(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"openai done", "data: [DONE]\n", true},
		{"openai finish", `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n", true},
		{"anthropic stop", `data: {"type":"message_stop"}` + "\n", true},
		{"anthropic delta", `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n", true},
		{"anthropic content", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n", false},
	}
	for _, tc := range cases {
		var m meter
		m.sseLine([]byte(tc.line))
		if m.done != tc.want {
			t.Errorf("%s: done=%v, want %v", tc.name, m.done, tc.want)
		}
	}
}
