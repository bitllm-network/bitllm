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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bitllm-network/bitllm/internal/models"
	"github.com/bitllm-network/bitllm/internal/netmap"
	"github.com/bitllm-network/bitllm/internal/node"
	"github.com/bitllm-network/bitllm/internal/tui"
	"github.com/bitllm-network/bitllm/internal/update"
	"github.com/libp2p/go-libp2p/core/peer"
)

// DefaultBootstrap is used when no --bootstrap is given. The /dnsaddr/ name
// is the seed list: every TXT record at _dnsaddr.bootstrap.bitllm.org names
// an independent bootstrap node, so seeds can be added or retired without a
// release. The literal entry is the old Cloudflare Tunnel seed, kept as a
// fallback while the direct seeds are being deployed; remove it once two
// direct seeds answer. See docs/bootstrap.md.
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
	flag.Var(&modelFlags, "model", "local-name or local-name=public-name to share (repeatable; default: every upstream model)")
	flag.IntVar(&cfg.Concurrency, "concurrency", 1, "requests served in parallel")
	flag.Var(&boot, "bootstrap", "multiaddr of a known peer, /ip4/.../tcp/4001/p2p/12D3..., or a /dnsaddr/ seed name (repeatable; default: the DNS seed list plus boot.bitllm.org)")
	flag.StringVar(&gateway, "gateway", "127.0.0.1:7778", "local OpenAI-compatible gateway address")
	flag.IntVar(&port, "port", 4001, "p2p port, TCP and UDP (QUIC)")
	flag.IntVar(&wsPort, "ws-port", 0, "also accept libp2p over WebSocket on 127.0.0.1:PORT, for an HTTPS tunnel (Cloudflare Tunnel etc.) in front of a bootstrap node")
	flag.Var(&roots, "roots", "root account of the invite tree (repeatable; default: the BitLLM root). For private networks and tests")
	flag.Var(&announce, "announce", "public multiaddr to advertise, e.g. /dns4/boot.example.com/tcp/443/wss (repeatable)")
	flag.StringVar(&cfg.DataDir, "data", filepath.Join(home, ".bitllm"), "identity and ledger directory")
	flag.BoolVar(&cfg.DHTServer, "dht-server", false, "always act as a DHT server (for bootstrap nodes with a public address)")
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

	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	cfg.Bootstrap = boot
	cfg.Roots = DefaultRoots
	if len(roots) > 0 {
		cfg.Roots = roots
	}
	if len(boot) == 0 && !cfg.DHTServer {
		cfg.Bootstrap = DefaultBootstrap
	}
	cfg.Private = !cfg.DHTServer
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
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
		log.Fatal(err)
	}
	log.Printf("bitllm %s, peer id %s", node.Version, n.ID())
	for _, a := range n.Addrs() {
		log.Printf("listening %s", a)
	}
	if cfg.Upstream != "" {
		log.Printf("sharing %v from %s", cfg.Models, cfg.Upstream)
	}
	if cfg.Audit {
		if len(cfg.Auditors) > 0 && len(auditors) == len(node.DefaultAuditors) && auditors[0] == node.DefaultAuditors[0] {
			log.Printf("audit: on; preferred auditor %v is the seed run by the BitLLM maintainers (replace it with --auditor PEER_ID, drop it with --auditor none)", cfg.Auditors)
		} else {
			log.Printf("audit: on; preferred auditors %v", cfg.Auditors)
		}
	}
	log.Printf("gateway http://%s/v1", gateway)
	srv := &http.Server{Addr: gateway, Handler: n.Gateway()}
	go func() { <-ctx.Done(); srv.Close() }()
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
		go upd.Run(ctx)
	}
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
