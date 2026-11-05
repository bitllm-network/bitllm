package main

import (
	"slices"
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
