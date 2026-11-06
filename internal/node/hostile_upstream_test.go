package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
)

// TestStallBodyClock: only time spent waiting on our server counts, so a slow
// reader downstream cannot make our server look stalled.
func TestStallBodyClock(t *testing.T) {
	var fired atomic.Int32
	pr, pw := io.Pipe()
	defer pw.Close()
	b := &stallBody{ReadCloser: pr, wait: 30 * time.Millisecond, cancel: func() {}, fired: func() { fired.Add(1); pw.Close() }}
	go pw.Write([]byte("x"))
	if _, err := b.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(90 * time.Millisecond) // the reader is busy elsewhere
	if fired.Load() != 0 {
		t.Fatal("the clock ran while nobody was waiting on the server")
	}
	if _, err := b.Read(make([]byte, 1)); err == nil || fired.Load() != 1 {
		t.Fatalf("a silent server did not fire the clock: %v, %d", err, fired.Load())
	}
}

// TestHeaderTimeout: a server that never answers costs one --upstream-timeout,
// marks the upstream down, logs once, and the next good check marks it up.
func TestHeaderTimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body) // the server notices a hang-up only after this
		<-r.Context().Done()
	}))
	defer up.Close()
	n := servedBy(t, up, Config{UpstreamTimeout: 200 * time.Millisecond})
	h := rawPeer(t, n, nil, "")
	logs := captureLog(t)
	s := openInfer(t, h, n, protocol.InferProto, chatReq(0), true)
	if _, hdr := readHdr(t, bufio.NewReader(s), s, 10*time.Second); hdr.Status != 502 || hdr.Error != msgUpstream {
		t.Fatalf("%+v", hdr)
	}
	eventually(t, 10*time.Second, "not marked down and up again", func() bool {
		l := logs.String()
		return strings.Contains(l, "marked down") && strings.Contains(l, "up=true")
	})
	if c := strings.Count(logs.String(), "marked down"); c != 1 {
		t.Fatalf("logged %d times: %s", c, logs)
	}
	eventually(t, 10*time.Second, "slot not freed", n.Idle)
}

// TestBodyStall: a server that stops sending loses the job after upstreamStall
// and is marked down even while another job keeps the health check from running.
// The peer sees a broken stream, not a clean end.
func TestBodyStall(t *testing.T) {
	setVar(t, &upstreamStall, 300*time.Millisecond)
	stalledGone := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		var b struct {
			Max float64 `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		io.Copy(io.Discard, r.Body) // the server notices a hang-up only after this
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		if b.Max == 1 { // says one thing, then nothing
			<-r.Context().Done()
			close(stalledGone)
			return
		}
		for { // keeps talking until released
			select {
			case <-release:
				return
			case <-time.After(50 * time.Millisecond):
				fmt.Fprint(w, ": x\n\n")
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer up.Close()
	defer free()
	n := servedBy(t, up, Config{Concurrency: 2})
	ask := func(max int) (*bufio.Reader, func() error) {
		h := rawPeer(t, n, nil, "")
		s := openInfer(t, h, n, protocol.InferProto2, inferReq{Path: "/chat/completions", Body: json.RawMessage(
			fmt.Sprintf(`{"model":"ds","stream":true,"max_tokens":%d,"messages":[]}`, max))}, true)
		br := bufio.NewReader(s)
		if _, hdr := readHdr(t, br, s, 10*time.Second); hdr.Status != 200 {
			t.Fatalf("%+v", hdr)
		}
		return br, func() error {
			s.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err := io.Copy(io.Discard, br)
			return err
		}
	}
	_, keepsTalking := ask(2)
	_ = keepsTalking
	_, stalled := ask(1)
	if err := stalled(); err == nil {
		t.Fatal("a stalled response ended like a complete one")
	}
	select {
	case <-stalledGone:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream request was not canceled")
	}
	eventually(t, 10*time.Second, "stalled job kept its slot", func() bool { return n.inflight.Load() == 1 })
	if n.upOK.Load() {
		t.Fatal("upstream still up while the only other job blocks the health check")
	}
	free()
	eventually(t, 10*time.Second, "the next good check did not restore the upstream", n.upOK.Load)
	eventually(t, 10*time.Second, "not idle", n.Idle)
}

// TestLocalHangUpReachesUpstream: the stall watchdog sits on the shared path, and
// a local client that hangs up still stops our server at once.
func TestLocalHangUpReachesUpstream(t *testing.T) {
	gone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		io.Copy(io.Discard, r.Body) // the server notices a hang-up only after this
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	}))
	defer up.Close()
	n := servedBy(t, up, Config{})
	gw := httptest.NewServer(n.Gateway())
	defer gw.Close()
	ctx, hang := context.WithCancel(t.Context())
	defer hang()
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"ds","stream":true,"messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("%q %v", line, err)
	}
	hang()
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("a local hang-up did not reach our server")
	}
	eventually(t, 10*time.Second, "not idle", n.Idle)
}
