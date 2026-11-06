package node

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// serveGateway runs n's gateway on a real listener, as main does.
func serveGateway(t *testing.T, n *Node) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: n.Gateway()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String()
}

// A request that is live when the stop begins finishes, a new one is refused,
// and the closers run after that, with the host already down.
func TestShutdownDrainsLiveRequest(t *testing.T) {
	up, release := heldUpstream(t)
	n := testNode(t, t.Context(), Config{Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	srv, addr := serveGateway(t, n)

	var closedHostOpen []bool
	n.closers = append(n.closers, func() { closedHostOpen = append(closedHostOpen, len(n.host.Network().ListenAddresses()) > 0) })

	resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"ds","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || !strings.Contains(line, "hi") {
		t.Fatalf("first line %q, %v", line, err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done <- n.Shutdown(ctx, srv)
	}()
	eventually(t, 5*time.Second, "closing was never set", n.closing.Load)

	// New connections are refused while the live one keeps going.
	eventually(t, 5*time.Second, "a new request was still taken during the drain", func() bool {
		c := &http.Client{Timeout: time.Second}
		r, err := c.Get("http://" + addr + "/status")
		if err == nil {
			r.Body.Close()
		}
		return err != nil
	})
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned with a request live: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if len(closedHostOpen) != 0 {
		t.Fatal("closers ran during the drain")
	}

	release()
	if _, err := body.ReadString(0); err == nil {
		t.Fatal("expected the end of the body")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the request finished")
	}
	if len(closedHostOpen) != 1 || closedHostOpen[0] {
		t.Fatalf("closers ran %d times, with the host open: %v", len(closedHostOpen), closedHostOpen)
	}
}

// When the drain runs out, a request still live is cut and the stop goes on.
func TestShutdownCutsAfterTheDrain(t *testing.T) {
	up, _ := heldUpstream(t)
	n := testNode(t, t.Context(), Config{Upstream: up.URL + "/v1", Models: map[string]string{"ds": "local-ds"}})
	srv, addr := serveGateway(t, n)
	resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"ds","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bufio.NewReader(resp.Body).ReadString('\n')

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := n.Shutdown(ctx, srv); err == nil {
		t.Fatal("no error for a drain that ran out")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
	eventually(t, 5*time.Second, "the cut request still counts as live", func() bool { return n.gatewayLive.Load() == 0 })
}

// Close waits for peer jobs, stops announcing at once, and can be called twice.
func TestCloseStopsAnnouncingAndWaitsForPeerJobs(t *testing.T) {
	up := mockUpstream(t, "local-m")
	defer up.Close()
	n := testNode(t, t.Context(), Config{Upstream: up.URL + "/v1", Models: map[string]string{"m": "local-m"}})
	eventually(t, 5*time.Second, "never announced", func() bool { return n.lastAnn.Load() != nil })

	n.inflight.Add(1) // a job on a slot
	done := make(chan error, 1)
	go func() { done <- n.Close(context.Background()) }()
	eventually(t, 5*time.Second, "closing was never set", n.closing.Load)

	// One announce may have been under way when the loops were stopped.
	time.Sleep(3 * n.cfg.AnnounceEvery)
	before := n.lastAnn.Load()
	time.Sleep(3 * n.cfg.AnnounceEvery)
	if n.lastAnn.Load() != before {
		t.Fatal("still announcing after the stop began")
	}
	select {
	case err := <-done:
		t.Fatalf("Close returned with a job running: %v", err)
	default:
	}
	n.inflight.Add(-1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the job ended")
	}
	if err := n.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
