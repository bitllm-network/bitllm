package node

import (
	"log"

	"github.com/libp2p/go-libp2p"
	libp2pproto "github.com/libp2p/go-libp2p/core/protocol"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	circuit "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

const (
	gib = uint64(1) << 30

	// defaultReservations is what a node that is not public configures. It never
	// runs the relay service in practice: such a node is told it is behind NAT.
	defaultReservations = 1024
)

// publicLimits is what a public node (--public or --dht-server) holds open. The
// library defaults do not fit one: the connection manager trims at 192, and the
// resource manager gives a 1 GB machine 64 inbound and 128 total connections,
// whatever the relay is configured to accept.
//
// The numbers scale with memory, from 0.5 to 4 times these for 1 GiB:
//
//	reservations  128   only tagged, so they must fit under the low watermark;
//	                    that leaves 200-128 = 72 places for ordinary peers
//	low/high      200/300  connections the manager keeps
//	inbound/total 400/500  above high, since a new connection cannot be trimmed
//	                    for its first minute
//	streams       8 per connection: 3200 inbound, 4000 total
//	memory        half the machine, 512 MiB
//
// What the memory limit allows: yamux reserves 256 KiB per stream on TCP, so
// 512 MiB is 2048 stream windows, or 300 connections with 6 streams each and
// some to spare. QUIC streams reserve nothing up front. The transient scope holds
// streams until their protocol is known: 266 inbound at 256 KiB is 66 MiB of its
// 128 MiB. The relay service takes 1024 streams, which is 512 circuits, since
// each uses two. Each circuit also holds about 12 KiB of buffers and messages,
// about 6 MiB in all, inside the service's 32 MiB.
//
// Those are reservations, not use: a window only fills when data waits. For
// scale, seed1 ran 7 connections at 44 MB. The 3/4 of memory that
// deploy/bootstrap-vps.sh gives its unit is above the 512 MiB here.
type publicLimits struct {
	TotalMemory     uint64 `json:"total_memory"`
	ConnLow         int    `json:"conn_low"`
	ConnHigh        int    `json:"conn_high"`
	ConnsInbound    int    `json:"conns_inbound"`
	ConnsOutbound   int    `json:"conns_outbound"`
	Conns           int    `json:"conns"`
	StreamsInbound  int    `json:"streams_inbound"`
	StreamsOutbound int    `json:"streams_outbound"`
	Streams         int    `json:"streams"`
	Memory          int64  `json:"memory"`
	Reservations    int    `json:"relay_reservations"`
	RelayStreams    int    `json:"relay_streams"`
}

// limitsForMemory sizes a public node for a machine with total bytes of memory.
// A machine whose memory cannot be read counts as 1 GiB.
func limitsForMemory(total uint64) publicLimits {
	known := total
	if known == 0 {
		known = gib
	}
	scale := min(max(float64(known)/float64(gib), 0.5), 4)
	n := func(perGiB int) int { return int(float64(perGiB) * scale) }
	return publicLimits{
		TotalMemory:     total,
		ConnLow:         n(200),
		ConnHigh:        n(300),
		ConnsInbound:    n(400),
		ConnsOutbound:   n(200),
		Conns:           n(500),
		StreamsInbound:  n(3200),
		StreamsOutbound: n(1600),
		Streams:         n(4000),
		Memory:          int64(min(known/2, 2*gib)),
		Reservations:    n(128),
		RelayStreams:    n(1024),
	}
}

// limitConfig turns the limits into the resource manager's. What is not named
// here keeps the library's scaled default, including file descriptors, which
// follow the process's own limit.
func (l publicLimits) limitConfig() rcmgr.ConcreteLimitConfig {
	scaled := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&scaled)
	lim := func(v int) rcmgr.LimitVal { return rcmgr.LimitVal(v) }
	relayStreams := rcmgr.ResourceLimits{
		StreamsInbound:  lim(l.RelayStreams),
		StreamsOutbound: lim(l.RelayStreams),
		Streams:         lim(l.RelayStreams),
		Memory:          rcmgr.LimitVal64(int64(l.RelayStreams) * 32 << 10),
	}
	return rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			ConnsInbound:    lim(l.ConnsInbound),
			ConnsOutbound:   lim(l.ConnsOutbound),
			Conns:           lim(l.Conns),
			StreamsInbound:  lim(l.StreamsInbound),
			StreamsOutbound: lim(l.StreamsOutbound),
			Streams:         lim(l.Streams),
			Memory:          rcmgr.LimitVal64(l.Memory),
		},
		// A restarted seed sees everyone redial within seconds, and everything
		// is transient until its peer or protocol is known.
		Transient: rcmgr.ResourceLimits{
			ConnsInbound:    lim(l.ConnsInbound / 6),
			Conns:           lim(l.Conns / 4),
			StreamsInbound:  lim(l.StreamsInbound / 12),
			StreamsOutbound: lim(l.StreamsOutbound / 6),
			Streams:         lim(l.Streams / 8),
			Memory:          rcmgr.LimitVal64(l.Memory / 4),
		},
		Service: map[string]rcmgr.ResourceLimits{relay.ServiceName: relayStreams},
		Protocol: map[libp2pproto.ID]rcmgr.ResourceLimits{
			circuit.ProtoIDv2Hop:  relayStreams,
			circuit.ProtoIDv2Stop: relayStreams,
		},
	}.Build(scaled.AutoScale())
}

// options are the host options that put the limits in force. If a manager
// cannot be built the node keeps the library's default: a limit must not stop a
// node from starting.
func (l publicLimits) options() []libp2p.Option {
	var opts []libp2p.Option
	if cm, err := connmgr.NewConnManager(l.ConnLow, l.ConnHigh); err != nil {
		log.Printf("connection limits: %v (using the defaults)", err)
	} else {
		opts = append(opts, libp2p.ConnectionManager(cm))
	}
	if rm, err := rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(l.limitConfig())); err != nil {
		log.Printf("resource limits: %v (using the defaults)", err)
	} else {
		opts = append(opts, libp2p.ResourceManager(rm))
	}
	return opts
}
