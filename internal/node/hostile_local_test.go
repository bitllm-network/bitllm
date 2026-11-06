package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
)

// TestLocalHoldsSlot: a local request for our own model holds a slot without
// being refused, so the announced free count is honest, a peer waits for it,
// and the slot goes to that peer when the local request ends.
func TestLocalHoldsSlot(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		var req struct {
			Max float64 `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Max == 7 { // the local request
			<-release
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
	}))
	defer up.Close()
	n := servedBy(t, up, Config{Concurrency: 1})
	gw := httptest.NewServer(n.Gateway())
	defer gw.Close()

	local := func() chan int {
		done := make(chan int, 1)
		go func() {
			resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"ds","max_tokens":7,"messages":[]}`))
			if err != nil {
				done <- -1
				return
			}
			resp.Body.Close()
			done <- resp.StatusCode
		}()
		return done
	}
	first, second := local(), local() // past --concurrency: never refused
	eventually(t, 10*time.Second, "local requests hold no slots", func() bool { return n.inflight.Load() == 2 })
	eventually(t, 10*time.Second, "announce still says free", func() bool {
		a := n.lastAnn.Load()
		return a != nil && a.Free == 0
	})

	h := rawPeer(t, n, nil, "")
	s := openInfer(t, h, n, protocol.InferProto2, chatReq(time.Minute), true)
	br := bufio.NewReader(s)
	if _, hdr := readHdr(t, br, s, 10*time.Second); hdr.Status != 202 {
		t.Fatalf("a peer did not wait for the local request: %+v", hdr)
	}
	close(release)
	for _, c := range []chan int{first, second} {
		if code := <-c; code != 200 {
			t.Fatalf("local request: %d", code)
		}
	}
	for {
		_, hdr := readHdr(t, br, s, 10*time.Second)
		if hdr.Status == 202 {
			continue
		}
		if hdr.Status != 200 {
			t.Fatalf("peer after the local request: %+v", hdr)
		}
		break
	}
	eventually(t, 10*time.Second, "not idle at the end", n.Idle)
}
