// bitllm: a peer-to-peer network of local LLMs. Every node is the same
// program: it can share an OpenAI-compatible server with the network and it
// runs a local gateway that answers from any peer. No coordinator.
//
//	bitllm --upstream http://localhost:8080/v1 --bootstrap /ip4/.../p2p/12D3...
//	curl localhost:7778/v1/models
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitllm-org/bitllm/internal/models"
	"github.com/bitllm-org/bitllm/internal/netmap"
	"github.com/bitllm-org/bitllm/internal/node"
	"github.com/bitllm-org/bitllm/internal/tui"
	"github.com/bitllm-org/bitllm/internal/update"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// DefaultBootstrap is used when no --bootstrap is given. The /dnsaddr/ name
// is the seed list: every TXT record at _dnsaddr.bootstrap.bitllm.org names
// an independent seed, so hosts can be added or retired without a release.
// The literal is the old Cloudflare Tunnel seed. It stays as a fallback until
// _dnsaddr resolves to a live seed, then it is removed. See docs/bootstrap.md.
var DefaultBootstrap = []string{
	"/dnsaddr/bootstrap.bitllm.org",
	"/dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib",
}

// DefaultRoots are the accounts at the top of the invite tree (zengjiajun.eth).
// With none, membership is off and every node is treated as a member.
var DefaultRoots = []string{"0xf3f527b70af6012f328d3ede7fcd3f5f3ca6cba2"}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// joinAddrs picks the addresses another machine can try: public addresses
// first, then the best LAN one, one per IP and at most two. A join code is
// for a person to copy, not a dump of every interface the node listens on.
func joinAddrs(addrs []string) []string {
	var public4, public6 []string
	bestLAN, bestRank := "", 4
	seen := map[string]bool{}
	for _, a := range addrs {
		m, err := ma.NewMultiaddr(a)
		if err != nil {
			continue
		}
		s, ipErr := ipOf(m)
		key := s
		if ipErr != nil {
			key = a // a hostname has no IP to dedupe on
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if ipErr != nil {
			// A DNS address was announced on purpose (a hostname, or a tunnel
			// in front of this node), so it is public by definition.
			switch {
			case hasProto(m, ma.P_DNS4):
				if len(public4) < 2 {
					public4 = append(public4, a)
				}
			case hasProto(m, ma.P_DNS6):
				if len(public6) < 2 {
					public6 = append(public6, a)
				}
			}
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		switch {
		case dialable(ip) && ip.To4() != nil:
			if len(public4) < 2 {
				public4 = append(public4, a)
			}
		case dialable(ip):
			if len(public6) < 2 {
				public6 = append(public6, a)
			}
		case lanUsable(ip):
			// Prefer a home LAN over a container bridge: 192.168, then 10,
			// then the 172.16/12 range Docker picks from.
			if r := lanRank(ip); r < bestRank {
				bestRank, bestLAN = r, a
			}
		}
	}
	// IPv4 first: a public IPv6 is often blocked by the home router, while a
	// public IPv4 almost always works.
	if out := append(public4, public6...); len(out) > 0 {
		if len(out) > 2 {
			out = out[:2]
		}
		return out
	}
	if bestLAN != "" {
		return []string{bestLAN}
	}
	return nil
}

// ipOf is the IP component of a /ip4/ or /ip6/ multiaddr.
func ipOf(m ma.Multiaddr) (string, error) {
	if v, err := m.ValueForProtocol(ma.P_IP4); err == nil {
		return v, nil
	}
	return m.ValueForProtocol(ma.P_IP6)
}

func hasProto(m ma.Multiaddr, p int) bool {
	_, err := m.ValueForProtocol(p)
	return err == nil
}

// dialable: a stranger could try this address. CGNAT (100.64.0.0/10) looks
// public but cannot be reached from outside.
func dialable(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() &&
		!ip.IsPrivate() && !cgnat(ip)
}

func lanUsable(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !cgnat(ip)
}

func cgnat(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

func lanRank(ip net.IP) int {
	v4 := ip.To4()
	switch {
	case v4 == nil:
		return 2 // IPv6 ULA: works on the LAN, but less often shared
	case v4[0] == 192 && v4[1] == 168:
		return 0
	case v4[0] == 10:
		return 1
	default:
		return 3
	}
}

// gatewayExposed reports whether a --gateway address listens beyond this
// machine. An empty host means every interface, and a name other than
// localhost may resolve to anything.
func gatewayExposed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = strings.Trim(addr, "[]")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return !ip.IsLoopback()
	}
	return host != "localhost"
}

// concurrencyWarning is for values announces cannot carry: peers drop this
// node's announces, so the flag is worth a warning, and never a failed start: a
// self-updated node that exits at startup comes back and exits again.
func concurrencyWarning(c int) string {
	if c >= 1 && c <= 256 {
		return ""
	}
	return fmt.Sprintf("WARNING: --concurrency %d is outside 1 to 256; peers will ignore this node's announces", c)
}

// logJoinCode says how another machine can join this node.
func logJoinCode(logf func(string, ...any), addrs []string) {
	codes := joinAddrs(addrs)
	if len(codes) == 0 {
		return
	}
	logf("another machine joins it with: bitllm --bootstrap %s", codes[0])
	for _, a := range codes[1:] {
		logf("or with another address: bitllm --bootstrap %s", a)
	}
}

// joinCodeAfter logs the join code once, if no seed has connected after wait.
// Seeds resolve to something by default, so an empty list says little: a node
// cut off from all of them is the one that needs another way in.
func joinCodeAfter(ctx context.Context, wait time.Duration, connected func() bool, addrs func() []string, logf func(string, ...any)) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(wait):
	}
	if connected() {
		return
	}
	logf("no seed connected after %v; still trying, and this node is on its own until one answers", wait)
	logJoinCode(logf, addrs())
}

// signals turns the process's signals into a shutdown. Before the node serves,
// the first signal cancels the start. After that it runs stop, and a second
// signal exits at once, whatever stop is waiting for.
type signals struct {
	cancel context.CancelFunc
	exit   func(int)

	mu    sync.Mutex
	stop  func()
	fired bool
}

func (s *signals) watch(c <-chan os.Signal) {
	<-c
	go func() {
		<-c
		log.Printf("second signal: exiting now")
		s.exit(130)
	}()
	s.mu.Lock()
	s.fired = true
	stop := s.stop
	s.mu.Unlock()
	if stop == nil {
		s.cancel()
		return
	}
	stop()
}

// serving hands over the shutdown to run from now on. It returns false if a
// signal came first, and then the caller should just return.
func (s *signals) serving(stop func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = stop
	return !s.fired
}

func main() {
	home, _ := os.UserHomeDir()
	if len(os.Args) > 1 && os.Args[1] == "account" {
		accountCmd(os.Args[2:], home)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "invite" {
		inviteCmd(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hash-model" {
		hashModelCmd(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "top" {
		tui.Run(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "update" {
		updateCmd(os.Args[2:])
		return
	}
	var (
		cfg              node.Config
		modelFlags, boot listFlag
		announce         listFlag
		roots            listFlag
		allow            listFlag
		weights          listFlag
		auditors         listFlag
		wsPort           int
		gateway          string
		port             int
		ipv6             bool
	)
	flag.StringVar(&cfg.Upstream, "upstream", "", "OpenAI-compatible base URL to share, e.g. http://localhost:8080/v1 (empty: use the network only)")
	flag.StringVar(&cfg.UpstreamKey, "upstream-key", os.Getenv("BITLLM_UPSTREAM_KEY"), "API key for the upstream (default $BITLLM_UPSTREAM_KEY, which keeps it out of the process list)")
	flag.Var(&modelFlags, "model", "local-name or local-name=public-name to share (repeatable; default: every upstream model, refreshed while the node runs)")
	flag.IntVar(&cfg.Concurrency, "concurrency", 1, "requests served in parallel")
	flag.Var(&boot, "bootstrap", "multiaddr of a known peer, /ip4/.../tcp/4001/p2p/12D3..., or a /dnsaddr/ seed name (repeatable; default: the DNS seed list plus boot.bitllm.org)")
	flag.StringVar(&gateway, "gateway", "127.0.0.1:7778", "local OpenAI-compatible gateway address")
	flag.StringVar(&cfg.GatewayKey, "gateway-key", os.Getenv("BITLLM_GATEWAY_KEY"), "key every gateway request except the dashboard page must carry, as Authorization: Bearer or x-api-key; open the dashboard once with ?key=KEY (default $BITLLM_GATEWAY_KEY, which keeps it out of the process list)")
	flag.IntVar(&port, "port", 4001, "p2p port, TCP and UDP (QUIC)")
	flag.IntVar(&wsPort, "ws-port", 0, "also accept libp2p over WebSocket on 127.0.0.1:PORT, for an HTTPS tunnel (Cloudflare Tunnel etc.) in front of a bootstrap node")
	flag.Var(&roots, "roots", "root account of the invite tree (repeatable; default: the BitLLM root). For private networks and tests")
	flag.Var(&announce, "announce", "public multiaddr to advertise, e.g. /dns4/boot.example.com/tcp/443/wss (repeatable)")
	flag.StringVar(&cfg.DataDir, "data", filepath.Join(home, ".bitllm"), "identity and ledger directory")
	flag.BoolVar(&cfg.DHTServer, "dht-server", false, "act as a seed: public DHT server and relay, and do not fall back to the default seeds (for the first bootstrap node)")
	flag.BoolVar(&cfg.Public, "public", false, "act as a public DHT server and relay, but still join the default seeds (for a host with inbound TCP and UDP)")
	flag.BoolVar(&cfg.MDNS, "mdns", true, "find peers on the local network")
	flag.BoolVar(&ipv6, "ipv6", true, "accept p2p connections over IPv6 too; --ipv6=false listens on IPv4 only, so a public IPv6 address does not expose the p2p port")
	flag.BoolVar(&cfg.UPnP, "upnp", true, "ask the router (UPnP/NAT-PMP) to forward the p2p port so peers can reach us directly")
	flag.IntVar(&cfg.MaxTokens, "max-tokens", 32768, "most new tokens a peer may ask for in one request (0: no limit)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Var(&allow, "allow", "peer id to serve (repeatable). With any --allow, this server serves only the listed peers, on any free slot like members, and refuses every other peer with a clear error")
	flag.BoolVar(&cfg.Open, "open", false, "serve uninvited nodes like members (any free slot, --free-daily per node), not only when idle")
	flag.IntVar(&cfg.OpenSlots, "open-slots", 0, "with --open: most slots uninvited nodes may hold at once (default: half of --concurrency, at least 1)")
	flag.Int64Var(&cfg.OpenDaily, "open-daily", 0, "with --open: credits all uninvited nodes together may use per UTC day, after which they are served only when idle (default 0: no limit)")
	flag.Int64Var(&cfg.FreeDaily, "free-daily", 0, "credits each member may use per UTC day on this server; contributors are exempt (default 0: no limit)")
	flag.Var(&weights, "weights", "model=HASH: state which weights your server loads for a shared model, as printed by `bitllm hash-model DIR` (repeatable). Announced as your claim; clients skip a server whose claim differs from the network's record")
	flag.BoolVar(&cfg.Audit, "audit", false, "spot-check the peers that answer you: now and then send one a canary request and have another node score the answer; a peer that keeps failing is not used for that model for 24 h (GET /audits shows the results)")
	flag.IntVar(&cfg.AuditEvery, "audit-every", 50, "with --audit: real requests to a peer between its spot checks (each peer is also checked on first use)")
	flag.Var(&auditors, "auditor", "with --audit: peer id of an auditor to try first (repeatable; default: the BitLLM maintainers' node; none: no preferred auditor)")
	flag.BoolVar(&cfg.AuditServe, "audit-serve", false, "offer to be an auditor: if your server returns prompt logprobs (vLLM), announce that this node can score answers for other nodes' --audit (they pay for it like any request)")
	location := flag.String("location", "", "where to show this node on the bitllm.org map: LAT,LON of any point in your city, e.g. 52.52,13.40. Only a ~156 km cell is announced, never the point. Default: not shown")
	autoUpdate := flag.Bool("auto-update", true, "check for a newer release after startup, install it, and restart into it when the node is idle; --auto-update=false stays on this version")
	flag.Parse()
	if *showVersion {
		fmt.Println("bitllm", node.Version)
		return
	}

	// Keep the last log lines in memory as well, so `bitllm top` and GET /logs
	// can show what the node has been doing without reading a terminal.
	log.SetOutput(io.MultiWriter(os.Stderr, node.LogOutput))

	if w := concurrencyWarning(cfg.Concurrency); w != "" {
		log.Print(w)
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	cfg.GatewayKey = strings.TrimSpace(cfg.GatewayKey)
	cfg.Bootstrap = boot
	cfg.Roots = DefaultRoots
	if len(roots) > 0 {
		cfg.Roots = roots
	}
	if len(boot) == 0 && !cfg.DHTServer {
		cfg.Bootstrap = DefaultBootstrap
	}
	cfg.Private = !(cfg.DHTServer || cfg.Public)
	cfg.AnnounceEvery = 10 * time.Second
	cfg.Listen = node.ListenAddrs(port, ipv6)
	if len(allow) > 0 {
		cfg.Allow = map[peer.ID]bool{}
		for _, s := range allow {
			id, err := peer.Decode(s)
			if err != nil {
				log.Fatalf("--allow %s: not a peer id (bitllm prints its own at startup, and GET /status shows it)", s)
			}
			cfg.Allow[id] = true
		}
		log.Printf("serving only %d allowed peer(s)", len(cfg.Allow))
	}
	if wsPort > 0 {
		// Peers arriving through the tunnel all come from 127.0.0.1.
		cfg.RelayBehindProxy = true
		cfg.Listen = append(cfg.Listen, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/ws", wsPort))
	}
	cfg.Announce = announce
	loc, err := netmap.ParseLocation(*location)
	if err != nil {
		log.Fatalf("--location %s: %v", *location, err)
	}
	cfg.Location = loc
	if len(weights) > 0 {
		cfg.Weights = map[string]string{}
		for _, w := range weights {
			m, h, _ := strings.Cut(w, "=")
			if !models.ValidHash(h) {
				log.Fatalf("--weights %s: want MODEL=HASH, the 64 hex digits that bitllm hash-model prints", w)
			}
			cfg.Weights[m] = h
		}
	}
	if len(auditors) == 0 {
		auditors = node.DefaultAuditors
	}
	for _, s := range auditors {
		if s == "none" {
			cfg.Auditors = nil
			break
		}
		id, err := peer.Decode(s)
		if err != nil {
			log.Fatalf("--auditor %s: not a peer id", s)
		}
		cfg.Auditors = append(cfg.Auditors, id)
	}
	if len(modelFlags) > 0 {
		cfg.Models = map[string]string{}
		for _, m := range modelFlags {
			local, public, _ := strings.Cut(m, "=")
			if public == "" {
				public = local
			}
			cfg.Models[public] = local
		}
		cfg.ModelsFixed = true
	}

	// The node's context ends last, once it has drained. The updater's ends at
	// the first signal: a restart must not undo a request to stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, stopUpdates := context.WithCancel(ctx)
	defer stopUpdates()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	lifecycle := &signals{cancel: cancel, exit: os.Exit}
	go lifecycle.watch(sigs)
	// The updater is wired before the node, so GET /status can report it.
	// Only a release build takes part; dev and CI builds never replace
	// themselves.
	exe, _ := executable()
	var upd *update.Runner
	if *autoUpdate && update.IsRelease(node.Version) && exe != "" {
		upd = &update.Runner{Current: node.Version, Executable: exe, DataDir: cfg.DataDir, Client: update.New()}
		cfg.UpdateStatus = upd.Status
	}
	n, err := node.New(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return // a signal during startup
		}
		log.Fatal(err)
	}
	log.Printf("bitllm %s, peer id %s", node.Version, n.ID())
	for _, a := range n.Addrs() {
		log.Printf("listening %s", a)
	}
	if cfg.DHTServer {
		log.Printf("--dht-server: advertising this node as a public seed; inbound TCP and UDP %d must work", port)
	} else if cfg.Public {
		log.Printf("--public: advertising this node as a public relay; inbound TCP and UDP %d must work", port)
	}
	if len(n.Seeds()) == 0 {
		log.Printf("no seed peers: this node is alone on its own network")
		logJoinCode(log.Printf, n.Addrs())
	} else {
		go joinCodeAfter(ctx, 30*time.Second, n.SeedConnected, n.Addrs, log.Printf)
	}
	if cfg.Upstream != "" {
		if len(cfg.Models) > 0 {
			log.Printf("sharing %v from %s", cfg.Models, cfg.Upstream)
		} else {
			log.Printf("sharing every model from %s (refreshed while the node runs)", cfg.Upstream)
		}
	}
	if cfg.Audit {
		if len(cfg.Auditors) > 0 && len(auditors) == len(node.DefaultAuditors) && auditors[0] == node.DefaultAuditors[0] {
			log.Printf("audit: on; preferred auditor %v is the seed run by the BitLLM maintainers (replace it with --auditor PEER_ID, drop it with --auditor none)", cfg.Auditors)
		} else {
			log.Printf("audit: on; preferred auditors %v", cfg.Auditors)
		}
	}
	log.Printf("gateway http://%s/v1", gateway)
	if cfg.GatewayKey != "" {
		log.Printf("gateway key on: clients send it as their API key; open the dashboard once at http://%s/?key=YOUR_KEY", gateway)
	} else if gatewayExposed(gateway) {
		log.Printf("WARNING: GATEWAY %s IS OPEN TO OTHER MACHINES WITH NO KEY. Set --gateway-key or BITLLM_GATEWAY_KEY, or listen on 127.0.0.1.", gateway)
	}
	srv := &http.Server{Addr: gateway, Handler: n.Gateway()}
	stopped := make(chan struct{})
	if !lifecycle.serving(func() {
		defer close(stopped)
		log.Printf("stopping: finishing live requests for up to %v (signal again to exit now)", node.Drain)
		stopUpdates()
		drain, done := context.WithTimeout(context.Background(), node.Drain)
		defer done()
		if err := n.Shutdown(drain, srv); err != nil {
			log.Printf("shutdown: %v", err)
		}
		cancel()
	}) {
		return
	}
	if upd != nil {
		upd.Idle = n.Idle
		if update.InPlace {
			// The listening sockets close on exec, so nothing has to be shut
			// down first, and a failed exec leaves the node serving.
			upd.Restart = func(path string) error {
				log.Printf("update: restarting into %s", path)
				return update.Restart(path, append([]string{path}, os.Args[1:]...), os.Environ())
			}
		}
		go upd.Run(updates)
	}
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	// Shutdown makes ListenAndServe return at once, so wait for the drain.
	<-stopped
}
