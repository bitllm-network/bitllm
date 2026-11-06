package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
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
