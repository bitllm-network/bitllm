package node

// Bootstrap peers are the only addresses a new node needs. A plain multiaddr
// names one peer. A /dnsaddr/ name is a seed list: every TXT record at
// _dnsaddr.<name> holds one dnsaddr= multiaddr, so several independent hosts
// can be published under one name and changed without a release. DNS is used
// only for the first contact; after that the DHT and gossip carry the network.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
)

// bootstrapDNSDepth bounds nested /dnsaddr/ names, e.g. a TXT record that
// points at another seed name.
const bootstrapDNSDepth = 3

var bootstrapResolver = madns.DefaultResolver

// ResolveBootstrap parses --bootstrap values and expands /dnsaddr/ names.
// A broken DNS name is logged and skipped, so one unreachable seed does not
// stop the others; an invalid plain multiaddr is still a user error.
func ResolveBootstrap(ctx context.Context, addrs []string) ([]peer.AddrInfo, error) {
	var infos []peer.AddrInfo
	for _, s := range addrs {
		m, err := ma.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("bootstrap %q: %w", s, err)
		}
		if !hasDNSAddr(m) {
			ais, err := peer.AddrInfosFromP2pAddrs(m)
			if err != nil {
				return nil, fmt.Errorf("bootstrap %q: %w", s, err)
			}
			infos = append(infos, ais...)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		resolved, err := resolveDNSAddr(rctx, m, bootstrapDNSDepth)
		cancel()
		if err != nil {
			log.Printf("bootstrap %s: %v", s, err)
			continue
		}
		for _, r := range resolved {
			ais, err := peer.AddrInfosFromP2pAddrs(r)
			if err != nil {
				log.Printf("bootstrap %s: %v", r, err)
				continue
			}
			infos = append(infos, ais...)
		}
	}
	return mergeAddrInfos(infos), nil
}

func hasDNSAddr(m ma.Multiaddr) bool {
	for _, p := range m.Protocols() {
		if p.Code == ma.P_DNSADDR {
			return true
		}
	}
	return false
}

func resolveDNSAddr(ctx context.Context, m ma.Multiaddr, depth int) ([]ma.Multiaddr, error) {
	if depth <= 0 {
		return nil, fmt.Errorf("%s: nested /dnsaddr/ names too deep", m)
	}
	out, err := bootstrapResolver.Resolve(ctx, m)
	if err != nil {
		return nil, err
	}
	var res []ma.Multiaddr
	for _, a := range out {
		if hasDNSAddr(a) {
			nested, err := resolveDNSAddr(ctx, a, depth-1)
			if err != nil {
				log.Printf("bootstrap %s: %v", a, err)
				continue
			}
			res = append(res, nested...)
			continue
		}
		res = append(res, a)
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("%s: no usable dnsaddr records", m)
	}
	return res, nil
}

// withoutPeer drops id from the bootstrap list. A seed that is also published
// in _dnsaddr would otherwise dial and reserve on itself.
func withoutPeer(in []peer.AddrInfo, id peer.ID) []peer.AddrInfo {
	out := in[:0]
	for _, ai := range in {
		if ai.ID != id {
			out = append(out, ai)
		}
	}
	return out
}

// mergeAddrInfos keeps one AddrInfo per peer and every distinct address, so
// tcp and quic addresses of one seed become one relay, not two.
func mergeAddrInfos(in []peer.AddrInfo) []peer.AddrInfo {
	var out []peer.AddrInfo
	index := map[peer.ID]int{}
	seen := map[peer.ID]map[string]bool{}
	for _, ai := range in {
		i, ok := index[ai.ID]
		if !ok {
			index[ai.ID] = len(out)
			out = append(out, peer.AddrInfo{ID: ai.ID})
			seen[ai.ID] = map[string]bool{}
			i = len(out) - 1
		}
		for _, a := range ai.Addrs {
			s := a.String()
			if seen[ai.ID][s] {
				continue
			}
			seen[ai.ID][s] = true
			out[i].Addrs = append(out[i].Addrs, a)
		}
	}
	return out
}
