package node

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/network"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	basicconnmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/pbnjay/memory"
)

// yamuxWindow is what yamux reserves per stream on TCP.
const yamuxWindow = 256 << 10

// TestLimitsForMemory: the numbers for a given machine, and how they relate.
func TestLimitsForMemory(t *testing.T) {
	one := limitsForMemory(gib)
	if one.ConnLow != 200 || one.ConnHigh != 300 || one.Reservations != 128 || one.Memory != 512<<20 {
		t.Fatalf("1 GiB: %+v", one)
	}
	if zero := limitsForMemory(0); zero.ConnHigh != 300 || zero.Memory != 512<<20 {
		t.Fatalf("unknown memory: %+v", zero)
	}
	if four := limitsForMemory(4 * gib); four.ConnHigh != 1200 || four.Reservations != 512 || four.Memory != 2*gib64 {
		t.Fatalf("4 GiB: %+v", four)
	}
	if big := limitsForMemory(256 * gib); big != withTotal(limitsForMemory(4*gib), 256*gib) {
		t.Fatalf("256 GiB: %+v", big)
	}
	for _, total := range []uint64{0, 256 << 20, 970 << 20, gib, 2 * gib, 4 * gib, 16 * gib, 256 * gib} {
		l := limitsForMemory(total)
		if l.Reservations >= l.ConnLow {
			t.Fatalf("%d: reservations %d leave no room under the low watermark %d", total, l.Reservations, l.ConnLow)
		}
		if l.ConnsInbound <= l.ConnHigh || l.Conns <= l.ConnsInbound {
			t.Fatalf("%d: the resource manager refuses what the connection manager keeps: %+v", total, l)
		}
		if l.StreamsInbound <= 6*l.ConnHigh {
			t.Fatalf("%d: too few streams: %+v", total, l)
		}
		if total > 0 && uint64(l.Memory) > total/2 {
			t.Fatalf("%d: memory %d is over half the machine", total, l.Memory)
		}
		if total >= 970<<20 && l.Memory/yamuxWindow < int64(6*l.ConnHigh) {
			t.Fatalf("%d: memory holds %d stream windows, want 6 per connection", total, l.Memory/yamuxWindow)
		}
	}
}

const gib64 = int64(gib)

func withTotal(l publicLimits, total uint64) publicLimits {
	l.TotalMemory = total
	return l
}

// TestLimitConfig: the resource manager gets the same numbers, and the relay
// service has room for its circuits.
func TestLimitConfig(t *testing.T) {
	l := limitsForMemory(gib)
	lim := rcmgr.NewFixedLimiter(l.limitConfig())
	sys := lim.GetSystemLimits()
	if sys.GetConnLimit(network.DirInbound) != 400 || sys.GetConnTotalLimit() != 500 ||
		sys.GetStreamLimit(network.DirInbound) != 3200 || sys.GetStreamTotalLimit() != 4000 || sys.GetMemoryLimit() != 512<<20 {
		t.Fatalf("system limits: %+v", sys)
	}
	if tr := lim.GetTransientLimits(); tr.GetConnLimit(network.DirInbound) != 66 || tr.GetStreamLimit(network.DirInbound) != 266 ||
		tr.GetMemoryLimit() != 128<<20 {
		t.Fatalf("transient limits: %+v", tr)
	}
	if svc := lim.GetServiceLimits(relay.ServiceName); svc.GetStreamTotalLimit() != 1024 {
		t.Fatalf("relay service limits: %+v", svc)
	}
}

// TestRelayResources: reservations follow the limits, and a relay behind a
// tunnel, where every peer has one IP, can give them all to that IP.
func TestRelayResources(t *testing.T) {
	if rc := relayResources(false, 128); rc.MaxReservations != 128 || rc.MaxReservationsPerIP != relay.DefaultResources().MaxReservationsPerIP {
		t.Fatalf("direct: %+v", rc)
	}
	if rc := relayResources(true, 128); rc.MaxReservations != 128 || rc.MaxReservationsPerIP != 128 {
		t.Fatalf("behind a proxy: %+v", rc)
	}
}

// TestPublicNodeLimitsInForce: a public node starts with the limits for this
// machine and shows them in /status. Any other node keeps the library defaults
// and shows none.
func TestPublicNodeLimitsInForce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := limitsForMemory(memory.TotalMemory())
	public := testNode(t, ctx, Config{})

	cm := public.host.ConnManager().(*basicconnmgr.BasicConnMgr).GetInfo()
	if cm.LowWater != want.ConnLow || cm.HighWater != want.ConnHigh {
		t.Fatalf("connection manager %d/%d, want %d/%d", cm.LowWater, cm.HighWater, want.ConnLow, want.ConnHigh)
	}
	if got := public.host.Network().ResourceManager().(connmgr.GetConnLimiter).GetConnLimit(); got != want.Conns {
		t.Fatalf("resource manager connections = %d, want %d", got, want.Conns)
	}
	got, ok := statusOf(t, public)["limits"].(map[string]any)
	if !ok || got["conn_high"] != float64(want.ConnHigh) || got["relay_reservations"] != float64(want.Reservations) ||
		got["memory"] != float64(want.Memory) {
		t.Fatalf("/status limits = %v, want %+v", got, want)
	}

	plain, err := New(ctx, Config{Private: true, DataDir: t.TempDir(), AnnounceEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.host.Close() })
	if cm := plain.host.ConnManager().(*basicconnmgr.BasicConnMgr).GetInfo(); cm.LowWater != 160 || cm.HighWater != 192 {
		t.Fatalf("a private node's connection manager changed: %d/%d", cm.LowWater, cm.HighWater)
	}
	if _, ok := statusOf(t, plain)["limits"]; ok {
		t.Fatal("a private node shows limits")
	}
}
