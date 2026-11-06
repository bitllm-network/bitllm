package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A join code a remote machine can use: public addresses first, LAN second,
// never loopback, one per IP.
func TestJoinAddrs(t *testing.T) {
	in := []string{
		"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
		"/ip4/192.168.1.5/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
		"/ip6/::1/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
		"/ip4/203.0.113.9/udp/4001/quic-v1/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
		"/ip4/203.0.113.9/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib", // same IP, dropped
	}
	want := []string{
		"/ip4/203.0.113.9/udp/4001/quic-v1/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
	}
	if got := joinAddrs(in); !slices.Equal(got, want) {
		t.Fatalf("joinAddrs = %v, want %v", got, want)
	}
}

func TestJoinAddrsSkipsUnreachable(t *testing.T) {
	in := []string{
		"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
		"/ip6/::1/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
		"/ip4/100.64.1.2/tcp/4001/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib", // CGNAT
	}
	if got := joinAddrs(in); len(got) != 0 {
		t.Fatalf("joinAddrs = %v, want none", got)
	}
}

// With no public address, show the home LAN, not a Docker bridge.
func TestJoinAddrsPrefersHomeLAN(t *testing.T) {
	const id = "12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib"
	in := []string{
		"/ip4/172.17.0.1/tcp/4001/p2p/" + id,
		"/ip4/10.0.0.5/tcp/4001/p2p/" + id,
		"/ip4/192.168.0.180/tcp/4001/p2p/" + id,
	}
	want := []string{"/ip4/192.168.0.180/tcp/4001/p2p/" + id}
	if got := joinAddrs(in); !slices.Equal(got, want) {
		t.Fatalf("joinAddrs = %v, want %v", got, want)
	}
}

// A tunneled seed announces a hostname, not an IP: that is the address a
// remote machine has to use.
func TestJoinAddrsKeepsAnnouncedHostname(t *testing.T) {
	const id = "12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib"
	in := []string{
		"/ip4/127.0.0.1/tcp/4001/p2p/" + id,
		"/dns4/boot.example.org/tcp/443/wss/p2p/" + id,
	}
	want := []string{"/dns4/boot.example.org/tcp/443/wss/p2p/" + id}
	if got := joinAddrs(in); !slices.Equal(got, want) {
		t.Fatalf("joinAddrs = %v, want %v", got, want)
	}
}

func TestGatewayExposed(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7778":          false,
		"127.1.2.3:7778":          false,
		"[::1]:7778":              false,
		"localhost:7778":          false,
		"0.0.0.0:7778":            true,
		"[::]:7778":               true,
		":7778":                   true,
		"192.168.1.5:7778":        true,
		"[fd00::1]:7778":          true,
		"my-box.lan:7778":         true,
		"203.0.113.9:7778":        true,
		"::ffff:10.0.0.1":         true,
		"127.0.0.1":               false,
		"[::ffff:127.0.0.1]:7778": false,
	} {
		if got := gatewayExposed(addr); got != want {
			t.Errorf("gatewayExposed(%q) = %v, want %v", addr, got, want)
		}
	}
}

// The CLI's POSTs reach a gateway that has a key.
func TestPostGatewaySendsTheKey(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	resp, err := postGateway(addr, " sekret\n", "/members", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer sekret" {
		t.Fatalf("Authorization = %q", got)
	}
	resp, err = postGateway(addr, "", "/members", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "" {
		t.Fatalf("Authorization without a key = %q", got)
	}
}

// Words from a pipe are read as a line, with or without a final newline.
func TestReadWordsFromAPipe(t *testing.T) {
	for _, in := range []string{"one two three\nleft over", "one two three"} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(in)
		w.Close()
		var out strings.Builder
		got, err := readWords("words: ", r, &out)
		r.Close()
		if err != nil || strings.TrimSpace(got) != "one two three" || out.String() != "words: " {
			t.Fatalf("readWords(%q) = %q, %v, printed %q", in, got, err, out.String())
		}
	}
}

// A node that takes the POST and never answers must not hang the command.
func TestPostGatewayTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	defer func(d time.Duration) { gatewayClient.Timeout = d }(gatewayClient.Timeout)
	if gatewayClient.Timeout <= 0 {
		t.Fatal("the CLI's HTTP client has no timeout")
	}
	gatewayClient.Timeout = 50 * time.Millisecond
	if resp, err := postGateway(strings.TrimPrefix(srv.URL, "http://"), "", "/members", []byte("{}")); err == nil {
		resp.Body.Close()
		t.Fatal("no timeout")
	}
}
