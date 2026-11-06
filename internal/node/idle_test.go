package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventually polls cond and fails the test if it does not hold in time.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s, after %v", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// heldUpstream streams one event for a chat request and then holds the
// response open until release is called or the request goes away.
func heldUpstream(t *testing.T) (up *httptest.Server, release func()) {
	t.Helper()
	held := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(held) }) }
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-held:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { release(); up.Close() })
	return up, release
}

// TestIdleCountsGatewayRequests: a self-update waits for Idle, so a local
// client in the middle of an answer must keep the node busy, whether the node
// answers from its own model or from a peer, until the answer ends or the
// client goes away.
func TestIdleCountsGatewayRequests(t *testing.T) {
	models := map[string]string{"ds": "local-ds"}
	paths := map[string]func(t *testing.T, ctx context.Context, upstream string) *Node{
		"own model": func(t *testing.T, ctx context.Context, upstream string) *Node {
			return testNode(t, ctx, Config{Upstream: upstream, Models: models})
		},
		"peer": func(t *testing.T, ctx context.Context, upstream string) *Node {
			server := testNode(t, ctx, Config{Upstream: upstream, Models: models})
			client := testNode(t, ctx, Config{Bootstrap: server.Addrs()[:1]})
			eventually(t, 20*time.Second, "client never discovered the server", func() bool {
				return len(client.candidates("ds")) > 0
			})
			return client
		},
	}
	for name, setup := range paths {
		for _, hangUp := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/hang up=%v", name, hangUp), func(t *testing.T) {
				up, release := heldUpstream(t)
				n := setup(t, t.Context(), up.URL+"/v1")
				gw := httptest.NewServer(n.Gateway())
				t.Cleanup(gw.Close)
				if !n.Idle() {
					t.Fatal("busy before any request")
				}

				reqCtx, hang := context.WithCancel(t.Context())
				defer hang()
				req, _ := http.NewRequestWithContext(reqCtx, "POST", gw.URL+"/v1/chat/completions",
					strings.NewReader(`{"model":"ds","stream":true,"messages":[]}`))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("status %d", resp.StatusCode)
				}
				br := bufio.NewReader(resp.Body)
				if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
					t.Fatalf("first event: %q, %v", line, err)
				}
				if n.Idle() {
					t.Fatal("idle with a client in the middle of an answer")
				}

				if hangUp {
					hang()
				} else {
					release()
					if _, err := io.Copy(io.Discard, br); err != nil {
						t.Fatalf("reading the rest of the answer: %v", err)
					}
				}
				eventually(t, 10*time.Second, "still busy after the request ended", n.Idle)
			})
		}
	}
}
