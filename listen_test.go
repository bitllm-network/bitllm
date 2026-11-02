package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/multiformats/go-multiaddr"
)

// TestListenAddrs: the default is exactly what nodes listened on before
// --ipv6 existed, and --ipv6=false drops every IPv6 address.
func TestListenAddrs(t *testing.T) {
	want := []string{
		"/ip4/0.0.0.0/tcp/4001",
		"/ip4/0.0.0.0/udp/4001/quic-v1",
		"/ip6/::/tcp/4001",
		"/ip6/::/udp/4001/quic-v1",
	}
	if got := listenAddrs(4001, true); !slices.Equal(got, want) {
		t.Fatalf("default changed:\n got %v\nwant %v", got, want)
	}
	if got := listenAddrs(4001, false); !slices.Equal(got, want[:2]) {
		t.Fatalf("--ipv6=false: %v", got)
	}
}

// TestNoIPv6Listener: a real host started with --ipv6=false has no IPv6
// listener, so nothing answers on its public IPv6 address.
func TestNoIPv6Listener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := NewNode(ctx, Config{Listen: listenAddrs(0, false), DataDir: t.TempDir(), Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer n.host.Close()
	addrs := n.host.Network().ListenAddresses()
	if len(addrs) == 0 {
		t.Fatal("no listeners")
	}
	for _, a := range addrs {
		if _, err := a.ValueForProtocol(multiaddr.P_IP6); err == nil || strings.HasPrefix(a.String(), "/ip6/") {
			t.Fatalf("IPv6 listener with --ipv6=false: %s", a)
		}
	}
}
