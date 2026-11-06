// Package node is one BitLLM node: the libp2p host, the DHT and gossip
// topics, the serving queue, the local OpenAI/Anthropic gateway, signed
// receipts, membership and auditing. cmd/bitllm builds one and serves it.
package node

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/ledger"
	"github.com/bitllm-org/bitllm/internal/members"
	"github.com/bitllm-org/bitllm/internal/models"
	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/bitllm-org/bitllm/internal/quota"
	"github.com/bitllm-org/bitllm/internal/receipts"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
)

const (
	// seedConnectTimeout bounds the first attempt to reach one seed.
	seedConnectTimeout = 10 * time.Second
	// syncEvery is how often a node syncs from a seed it already synced from.
	syncEvery = 10 * time.Minute
	// maxModelList caps the upstream's answer to GET /models.
	maxModelList = 4 << 20
)

// Config is everything a node needs from its operator. Zero values are the
// defaults described here, and New fills in the rest.
type Config struct {
	Upstream         string                // OpenAI-compatible base URL to share; empty = client only
	UpstreamKey      string                //
	GatewayKey       string                // when set, every gateway request but GET / must carry it
	Models           map[string]string     // public name -> upstream name; empty = discover
	ModelsFixed      bool                  // Models came from --model; do not replace them from upstream
	Concurrency      int                   //
	Listen           []string              // libp2p listen multiaddrs
	Bootstrap        []string              // multiaddrs with /p2p/ID
	DataDir          string                //
	DHTServer        bool                  // force DHT server mode (bootstrap seeds, tests)
	Public           bool                  // publicly reachable relay/DHT server that still joins the default seeds
	MDNS             bool                  //
	UPnP             bool                  // ask the router to forward our port (exposes the node publicly)
	Private          bool                  // assume we are behind NAT: reserve a slot on the bootstrap relays
	RelayBehindProxy bool                  // peers reach our relay through a tunnel, all from one IP
	Announce         []string              // extra public addrs, e.g. /dns4/boot.example.com/tcp/443/wss behind a tunnel
	FreeDaily        int64                 // credits a member may use per UTC day on this server; contributors are unlimited
	Roots            []string              // accounts at the top of the invite tree; none = membership off
	Open             bool                  // serve uninvited nodes like members (any free slot, quota per node)
	Allow            map[peer.ID]bool      // if set, serve only these peers (like members), and refuse every other peer
	OpenSlots        int                   // most slots uninvited nodes may hold on an open server; 0 = half
	OpenDaily        int64                 // credits all uninvited nodes together may use per UTC day on an open server; 0 = no limit
	MaxTokens        int                   // cap on new tokens per request we serve; 0 = none
	AnnounceEvery    time.Duration         //
	RedialEvery      time.Duration         // how often to reconnect a lost bootstrap; 0 = 15s
	SyncStall        time.Duration         // longest a seed may stay silent during a sync; 0 = 30s
	SyncLimit        time.Duration         // longest one sync from a seed may run; 0 = 5m
	ModelsTimeout    time.Duration         // longest one read of the upstream's model list may take; 0 = 10s
	Weights          map[string]string     // public name -> bitllm hash-model of the files our server loads (announced, not proven)
	Audit            bool                  // spot-check the peers that answer us (audit.go)
	AuditEvery       int                   // real requests to a peer between its spot checks
	Auditors         []peer.ID             // auditors tried first
	AuditServe       bool                  // announce the models our server can score, so others may use us as an auditor
	Location         string                // coarse geohash cell for the node map (network.go); empty = not shown
	UpdateStatus     func() map[string]any // what the updater reports, for GET /status; nil = no updater
}

// peerInfo is one peer's latest announce and when we heard it. The announce
// type lives in the protocol package so a reader can find the whole wire
// contract in one place.
type peerInfo struct {
	protocol.Announce
	seen  time.Time
	addrs []ma.Multiaddr // last addresses we saw for this peer, for relay candidates
}

// Node is a running node. Call New, then Gateway for the local API.
type Node struct {
	cfg          Config
	host         host.Host
	dht          *dht.IpfsDHT
	topic        *pubsub.Topic
	ledger       *ledger.Ledger
	receipts     *receipts.Receipts
	receiptTop   *pubsub.Topic
	claims       *account.Claims
	claimTop     *pubsub.Topic
	nodeKey      crypto.PrivKey
	members      *members.Members
	memTop       *pubsub.Topic
	memRate      originRate // membership gossip, per origin
	quota        *quota.Store
	accessMu     sync.Mutex
	accessAt     time.Time
	access       *members.MemberSet // cached, see memberSet
	servedMu     sync.Mutex         // own lock: memberSet holds accessMu while Compute calls servedSets
	servedAt     time.Time
	servedEver   map[string]bool // cached, see servedSets
	servedRecent map[string]bool
	annRate      originRate // announce gossip, per origin
	claimRate    originRate // claim gossip, per origin
	receiptSeed  []byte
	self         peer.ID
	relays       []peer.AddrInfo // bootstrap nodes, which also relay
	http         *http.Client
	modelMu      sync.RWMutex
	models       map[string]string // public name -> upstream name; replaced whole, never mutated
	inflight     atomic.Int32
	gatewayLive  atomic.Int32                      // completions the local gateway is answering, own model or proxied
	upOK         atomic.Bool                       // upstream answered its last health check
	lastAnn      atomic.Pointer[protocol.Announce] // what announceLoop last gossiped (GET /network counts us too)
	openInflight atomic.Int32                      // slots held by uninvited nodes on an open server
	imgMu        sync.Mutex
	imgLimit     map[string]int // upstream model -> most images its server takes, learned from a refusal
	audits       *audits        // nil without --audit

	mu    sync.Mutex
	peers map[peer.ID]*peerInfo

	// qmu guards the serving slots and the queue: inflight, openInflight,
	// peerSlots and waiters are changed under it (queue.go).
	qmu       sync.Mutex
	peerSlots map[peer.ID]int32
	waiters   []*waiter
	svc       atomic.Int64 // average job duration in ns, for the queue's ETA
	refused   refusals     // why this server refused requests, for /status

	started time.Time // when this node came up, shown by GET /status
}

// LoadKey reads the node key, creating it on first run.
func LoadKey(path string) (crypto.PrivKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		return crypto.UnmarshalPrivateKey(b)
	}
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, err
	}
	b, _ := crypto.MarshalPrivateKey(k)
	return k, os.WriteFile(path, b, 0o600)
}

// New starts a node: key, host, DHT, gossip, storage, then the loops. It does
// not wait for the network: the seeds are contacted in the background, so a
// seed that does not answer holds up neither the gateway nor the updater.
func New(ctx context.Context, cfg Config) (*Node, error) {
	if cfg.RedialEvery <= 0 {
		cfg.RedialEvery = 15 * time.Second
	}
	if cfg.SyncStall <= 0 {
		cfg.SyncStall = 30 * time.Second
	}
	if cfg.SyncLimit <= 0 {
		cfg.SyncLimit = 5 * time.Minute
	}
	if cfg.ModelsTimeout <= 0 {
		cfg.ModelsTimeout = 10 * time.Second
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	key, err := LoadKey(filepath.Join(cfg.DataDir, "identity.key"))
	if err != nil {
		return nil, err
	}
	self, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return nil, err
	}
	// Several addresses of one peer (tcp, quic...) must become one AddrInfo, or
	// the same node is dialed, synced and used as a relay once per address.
	bootstrap, err := ResolveBootstrap(ctx, cfg.Bootstrap)
	if err != nil {
		return nil, err
	}
	// A seed published in its own _dnsaddr list would dial itself. It resolves
	// to this identity, so drop it before it becomes a relay or a DHT peer.
	if clean := withoutPeer(bootstrap, self); len(clean) != len(bootstrap) {
		log.Printf("bootstrap: ignoring our own address")
		bootstrap = clean
	}

	// Both flags mean a public DHT server and relay. The difference is only
	// the first contact: --dht-server is a seed and may start alone, while
	// --public keeps the default seed list.
	public := cfg.DHTServer || cfg.Public
	n := &Node{
		cfg:       cfg,
		ledger:    ledger.Open(filepath.Join(cfg.DataDir, "ledger.json")),
		http:      &http.Client{},
		peers:     map[peer.ID]*peerInfo{},
		peerSlots: map[peer.ID]int32{},
		self:      self,
		relays:    bootstrap,
		models:    cfg.Models,
		started:   time.Now(),
	}
	// A caller that supplied a map (--model, or a test) pinned it. Only a
	// node that discovered its models keeps re-reading them from upstream.
	n.cfg.ModelsFixed = n.cfg.ModelsFixed || len(cfg.Models) > 0
	mode := dht.ModeAutoServer
	if public {
		mode = dht.ModeServer
	}
	opts := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(),
		// Public nodes relay for NATed ones until hole punching upgrades the link.
		// Inference streams are long, so the relay must not cut them off.
		libp2p.EnableRelayService(relay.WithResources(relayResources(cfg.RelayBehindProxy))),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			var err error
			n.dht, err = dht.New(h, dht.Mode(mode), dht.ProtocolPrefix("/bitllm"),
				dht.BootstrapPeers(bootstrap...))
			return n.dht, err
		}),
	}
	if len(cfg.Listen) > 0 {
		opts = append(opts, libp2p.ListenAddrStrings(cfg.Listen...))
	} else {
		opts = append(opts, libp2p.NoListenAddrs)
	}
	// AutoNAT needs several peers to vouch before it calls us private, which a
	// small network may never have; without that we would never take a relay
	// slot and nobody could reach us. So non-public nodes just assume NAT.
	// Public nodes (a seed or a public relay) are public by definition, and the
	// relay service only starts once we know we are public.
	if cfg.Private {
		opts = append(opts, libp2p.ForceReachabilityPrivate())
	} else if public {
		opts = append(opts, libp2p.ForceReachabilityPublic())
	}
	if len(cfg.Announce) > 0 {
		var extra []ma.Multiaddr
		for _, a := range cfg.Announce {
			m, err := ma.NewMultiaddr(a)
			if err != nil {
				return nil, fmt.Errorf("announce %q: %w", a, err)
			}
			extra = append(extra, m)
		}
		opts = append(opts, libp2p.AddrsFactory(func(as []ma.Multiaddr) []ma.Multiaddr {
			return append(as, extra...)
		}))
	}
	if cfg.UPnP {
		opts = append(opts, libp2p.NATPortMap())
	}
	if cfg.Private {
		// The source returns the seeds plus public peers that announced
		// relay=true, so a NATed node is not pinned to the seed's relay.
		// Seeds are cheap to reconsider, and a bootstrap that restarts takes
		// its reservation with it: look again soon, not after autorelay's
		// default 30 seconds, and do not sit out its one hour backoff after a
		// failed attempt during the restart.
		opts = append(opts, libp2p.EnableAutoRelayWithPeerSource(n.relaySource,
			autorelay.WithMinCandidates(1), autorelay.WithBootDelay(0),
			autorelay.WithMinInterval(5*time.Second), autorelay.WithBackoff(time.Minute)))
	}
	if n.host, err = libp2p.New(opts...); err != nil {
		return nil, err
	}

	if cfg.Upstream != "" {
		if len(n.modelMap()) == 0 {
			// A model server that is still starting is not fatal: the
			// announce loop keeps looking and shares its models when it
			// answers.
			discovered, err := n.discoverModels(ctx)
			if err != nil {
				log.Printf("upstream %s: %v (will retry)", cfg.Upstream, err)
			} else {
				n.setModels(discovered)
			}
		}
		n.host.SetStreamHandler(protocol.InferProto, n.handleInfer)
		n.host.SetStreamHandler(protocol.InferProto2, n.handleInfer)
	}

	n.nodeKey = key
	if n.receiptSeed, err = receipts.LoadSeed(filepath.Join(cfg.DataDir, "receipt.seed")); err != nil {
		return nil, err
	}
	n.claims = account.OpenClaims(filepath.Join(cfg.DataDir, "claims.json"))
	n.quota = quota.Open(filepath.Join(cfg.DataDir, "quota.json"))
	if n.receipts, err = receipts.Open(filepath.Join(cfg.DataDir, "receipts.jsonl"), key, n.host.ID()); err != nil {
		return nil, err
	}
	// After receipts and claims: replaying saved invites needs to know who served.
	n.members = members.Open(filepath.Join(cfg.DataDir, "members.json"), cfg.Roots, n.servedSets)
	n.host.SetStreamHandler(protocol.ReceiptProto, n.handleReceipt)
	n.host.SetStreamHandler(protocol.SyncProto, n.handleSync)
	n.host.SetStreamHandler(protocol.HelloProto, n.handleHello)

	ps, err := pubsub.NewGossipSub(ctx, n.host)
	if err != nil {
		return nil, err
	}
	ps.RegisterTopicValidator(protocol.ReceiptTopic, func(_ context.Context, _ peer.ID, msg *pubsub.Message) bool {
		var sr receipts.SignedReceipt
		return json.Unmarshal(msg.Data, &sr) == nil && n.receipts.AcceptGossip(msg.GetFrom(), &sr)
	})
	if n.receiptTop, err = ps.Join(protocol.ReceiptTopic); err != nil {
		return nil, err
	}
	rsub, err := n.receiptTop.Subscribe()
	if err != nil {
		return nil, err
	}
	go n.readReceipts(ctx, rsub)
	// A claim is gossiped by the node it names, at most once per 2 s (the node
	// re-gossips its claim 5 s after start and then every 5 min).
	n.claimRate.every = 2 * time.Second
	ps.RegisterTopicValidator(protocol.ClaimTopic, func(_ context.Context, _ peer.ID, msg *pubsub.Message) bool {
		var c account.Claim
		return json.Unmarshal(msg.Data, &c) == nil && c.Node == msg.GetFrom().String() &&
			c.Valid() && n.claimRate.allow(msg.GetFrom(), time.Now())
	})
	n.memRate.every = time.Second
	ps.RegisterTopicValidator(protocol.MembersTopic, func(_ context.Context, _ peer.ID, msg *pubsub.Message) bool {
		var it members.Item
		if json.Unmarshal(msg.Data, &it) != nil || !n.memRate.allow(msg.GetFrom(), time.Now()) {
			return false
		}
		// Cheap filter first: an invite from someone who is not a member (per
		// the 10 s cache) is dropped before the full check, which recomputes
		// the member set and costs CPU that outsiders must not be able to spend.
		if it.Invite != nil && n.members.Enabled() && !n.memberSet().Member[it.Invite.Inviter] {
			return false
		}
		return n.members.Known(it) || n.members.Check(it) == nil
	})
	if n.memTop, err = ps.Join(protocol.MembersTopic); err != nil {
		return nil, err
	}
	msub, err := n.memTop.Subscribe()
	if err != nil {
		return nil, err
	}
	go n.readMembers(ctx, msub)
	n.annRate.every = cfg.AnnounceEvery / 2
	ps.RegisterTopicValidator(protocol.AnnounceTopic, func(_ context.Context, _ peer.ID, msg *pubsub.Message) bool {
		var a protocol.Announce
		return json.Unmarshal(msg.Data, &a) == nil && protocol.ValidAnnounce(&a) && n.annRate.allow(msg.GetFrom(), time.Now())
	})
	if n.claimTop, err = ps.Join(protocol.ClaimTopic); err != nil {
		return nil, err
	}
	csub, err := n.claimTop.Subscribe()
	if err != nil {
		return nil, err
	}
	go n.readClaims(ctx, csub)
	go n.claimLoop(ctx)
	if n.topic, err = ps.Join(protocol.AnnounceTopic); err != nil {
		return nil, err
	}
	sub, err := n.topic.Subscribe()
	if err != nil {
		return nil, err
	}
	go n.readAnnounces(ctx, sub)

	if err := n.dht.Bootstrap(ctx); err != nil {
		return nil, err
	}
	if cfg.MDNS {
		mdns.NewMdnsService(n.host, protocol.Rendezvous, n).Start()
	}
	if cfg.Audit {
		n.audits = newAudits(n, cfg.AuditEvery, cfg.Auditors)
	}
	go n.joinSeeds(ctx)
	go n.redialLoop(ctx)
	if cfg.Upstream != "" {
		n.upOK.Store(n.checkUpstream(ctx))
		go n.announceLoop(ctx)
	}
	return n, nil
}

func (n *Node) readClaims(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		var c account.Claim
		if json.Unmarshal(msg.Data, &c) == nil {
			n.claims.Add(c)
		}
	}
}

// claimLoop re-gossips our own claim now and then, so peers that missed it
// (or joined after) learn who owns this node.
func (n *Node) claimLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if c, ok := n.claims.Get(n.host.ID().String()); ok {
			b, _ := json.Marshal(c)
			n.claimTop.Publish(ctx, b)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// Link records a claim of this node by owner, given the owner's signature
// over the claim message, countersigns it, and gossips it.
func (n *Node) Link(owner, name string, t int64, ownerSig string) (*account.Claim, error) {
	c, err := account.NewClaim(n.host.ID(), owner, name)
	if err != nil {
		return nil, err
	}
	c.Time, c.OwnerSig = t, ownerSig
	if err := account.SignClaimAsNode(c, n.nodeKey); err != nil {
		return nil, err
	}
	if !n.claims.Add(*c) {
		return nil, fmt.Errorf("claim rejected (invalid, or older than the current one)")
	}
	b, _ := json.Marshal(c)
	n.claimTop.Publish(context.Background(), b)
	return c, nil
}

// relayResources: no per-circuit time or byte limit (inference streams are
// long), but bounded slots so a public bootstrap is not an open-ended relay.
// The default cap of 8 reservations per source IP is the only sybil guard on a
// public-IP relay, but behind a tunnel every peer arrives from 127.0.0.1, so
// there it would let only 8 NATed nodes in the whole network be reachable.
func relayResources(behindProxy bool) relay.Resources {
	rc := relay.DefaultResources()
	rc.Limit = nil
	rc.MaxReservations = 1024 // NATed peers that can be reached through us
	rc.MaxCircuits = 64       // open relayed connections per peer
	if behindProxy {
		rc.MaxReservationsPerIP = rc.MaxReservations
	}
	return rc
}

// joinSeeds makes first contact with the network. Each seed gets its own
// goroutine that connects and then syncs, so a seed that does not answer delays
// neither the other seeds' connects nor their syncs. redialLoop retries the
// ones that fail here.
//
// The DHT loops start once the routing table has a peer, which happens a round
// trip after the first connect. A lookup on an empty table fails at once, and
// after that failure the rendezvous advertise sits out two minutes and
// provideLoop 15 seconds.
func (n *Node) joinSeeds(ctx context.Context) {
	for _, ai := range n.relays {
		go func() {
			c, cancel := context.WithTimeout(ctx, seedConnectTimeout)
			if err := n.host.Connect(c, ai); err != nil {
				log.Printf("bootstrap %s: %v", ai.ID, err)
			}
			cancel()
			n.syncLoop(ctx, ai.ID)
		}()
	}
	for deadline := time.Now().Add(seedConnectTimeout); n.dht.RoutingTable().Size() == 0 && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	go n.discoverLoop(ctx)
	if n.cfg.Upstream != "" {
		go n.provideLoop(ctx)
	}
}

// syncLoop re-syncs receipts, claims and invites from one seed now and then,
// so anything a node missed on gossip arrives eventually. While the seed is
// not connected it looks again every RedialEvery, so a node that started while
// the seed was unreachable catches up as soon as the connect lands, not ten
// minutes later. A failed sync is retried with a growing wait: the seed may
// still be starting, but a seed that keeps failing must not be asked for its
// whole history over and over.
func (n *Node) syncLoop(ctx context.Context, seed peer.ID) {
	retry := n.cfg.RedialEvery
	for {
		wait := n.cfg.RedialEvery
		if n.host.Network().Connectedness(seed) == network.Connected {
			added, err := n.syncReceipts(ctx, seed)
			if err != nil {
				log.Printf("receipt sync from %s: %v", seed, err)
				wait, retry = retry, min(retry*2, syncEvery)
			} else {
				if added > 0 {
					log.Printf("synced %d receipts from %s", added, seed)
				}
				wait, retry = syncEvery, n.cfg.RedialEvery
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// HandlePeerFound is the mDNS callback.
func (n *Node) HandlePeerFound(ai peer.AddrInfo) {
	if ai.ID != n.host.ID() {
		n.host.Connect(context.Background(), ai)
	}
}

func (n *Node) discoverLoop(ctx context.Context) {
	disc := drouting.NewRoutingDiscovery(n.dht)
	dutil.Advertise(ctx, disc, protocol.Rendezvous)
	for {
		if ch, err := disc.FindPeers(ctx, protocol.Rendezvous); err == nil {
			for ai := range ch {
				if ai.ID != n.host.ID() && n.host.Network().Connectedness(ai.ID) != network.Connected {
					n.host.Connect(ctx, ai)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// announceLoop gossips our models while the upstream is up. A node whose
// server is stopped (say, the GPU is busy with something else) goes quiet, so
// clients stop routing to it within three announce periods, and comes back on
// the first check that passes.
func (n *Node) announceLoop(ctx context.Context) {
	var scorable []string
	probed := false
	lastModels := time.Time{}
	for {
		// A job in flight already proves the server works, and a server that
		// handles one request at a time may not answer the probe meanwhile.
		if n.inflight.Load() == 0 {
			ok := n.checkUpstream(ctx)
			if ok != n.upOK.Swap(ok) {
				log.Printf("upstream %s: up=%v", n.cfg.Upstream, ok)
			}
			// A model server can add a model while we run (a new vLLM
			// deployment, a second model on the same host). Re-read the
			// list until the operator pins it with --model, so the network
			// sees the new model without a restart.
			if ok && !n.cfg.ModelsFixed && time.Since(lastModels) >= 30*time.Second {
				if m, err := n.discoverModels(ctx); err != nil {
					log.Printf("upstream %s: %v", n.cfg.Upstream, err)
				} else if !maps.Equal(m, n.modelMap()) {
					n.setModels(m)
					log.Printf("upstream models changed: %v", mapKeys(m))
				}
				lastModels = time.Now()
			}
		}
		models := n.modelNames()
		free := n.cfg.Concurrency - int(n.inflight.Load())
		a := protocol.Announce{Version: Version, Models: []string{}, Free: max(free, 0), Max: n.cfg.Concurrency, Q: n.queueDepth(), Relay: n.cfg.DHTServer || n.cfg.Public}
		if n.upOK.Load() {
			a.Models = models
			if n.cfg.AuditServe && !probed {
				scorable, probed = n.probeScoring(ctx), true
				if len(scorable) > 0 {
					log.Printf("our server can score %v: announcing it, so peers may use this node as an auditor", scorable)
				}
			}
			a.Audit = scorable
			a.Weights = n.cfg.Weights
			a.Loc = n.cfg.Location
		}
		n.lastAnn.Store(&a)
		b, _ := json.Marshal(a)
		n.topic.Publish(ctx, b)
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.cfg.AnnounceEvery):
		}
	}
}

// checkUpstream reports whether the upstream answers GET /models.
func (n *Node) checkUpstream(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", n.cfg.Upstream+"/models", nil)
	n.upstreamAuth(req)
	resp, err := n.http.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode/100 == 2
}

// modelMap returns the current public -> upstream map. The map is replaced
// whole, never mutated, so a caller may keep reading it after the lock is
// released.
func (n *Node) modelMap() map[string]string {
	n.modelMu.RLock()
	defer n.modelMu.RUnlock()
	return n.models
}

// setModels replaces the map atomically with respect to readers.
func (n *Node) setModels(m map[string]string) {
	n.modelMu.Lock()
	n.models = m
	n.modelMu.Unlock()
}

// modelNames returns the public names we serve, sorted.
func (n *Node) modelNames() []string {
	return mapKeys(n.modelMap())
}

// upstreamModel maps a public name to the name our local server knows.
func (n *Node) upstreamModel(public string) (string, bool) {
	n.modelMu.RLock()
	defer n.modelMu.RUnlock()
	up, ok := n.models[public]
	return up, ok
}

// ownModel reports whether we answer model ourselves right now.
func (n *Node) ownModel(model string) bool {
	_, ok := n.upstreamModel(model)
	return ok && n.upOK.Load()
}

func (n *Node) readAnnounces(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		from := msg.GetFrom() // origin (signed), not the forwarding hop
		if from == n.host.ID() {
			continue
		}
		var a protocol.Announce
		if json.Unmarshal(msg.Data, &a) != nil {
			continue
		}
		n.remember(from, a)
	}
}

// remember stores what a peer announced, evicting the least recently seen
// when the table is full.
func (n *Node) remember(from peer.ID, a protocol.Announce) {
	n.mu.Lock()
	if _, ok := n.peers[from]; !ok && len(n.peers) >= maxPeers {
		oldest, at := peer.ID(""), time.Now()
		for p, pi := range n.peers {
			if pi.seen.Before(at) {
				oldest, at = p, pi.seen
			}
		}
		delete(n.peers, oldest)
	}
	var addrs []ma.Multiaddr
	if n.host != nil {
		addrs = append(addrs, n.host.Peerstore().Addrs(from)...)
	}
	n.peers[from] = &peerInfo{Announce: a, seen: time.Now(), addrs: addrs}
	n.mu.Unlock()
	// The peer just reached us, so whatever address we failed to dial
	// before is worth trying again now. Without this, a relayed peer that
	// re-reserved after a bootstrap restart keeps its old address in the
	// swarm's dial backoff for minutes (issue #17).
	n.clearBackoff(from)
}

// clearBackoff forgets the dial backoff the swarm holds for p, so the next
// attempt goes out now instead of after a growing wait.
func (n *Node) clearBackoff(p peer.ID) {
	if s, ok := n.host.Network().(*swarm.Swarm); ok {
		s.Backoff().Clear(p)
	}
}

// modelKey is the DHT key a model's providers are recorded under. One key per
// model means a node that wants a model looks up its servers directly,
// instead of waiting for one of them to gossip at it.
func modelKey(model string) cid.Cid {
	sum := sha256.Sum256([]byte("bitllm/model/" + model))
	mh, err := multihash.Sum(sum[:], multihash.SHA2_256, -1)
	if err != nil {
		panic("model key: " + err.Error())
	}
	return cid.NewCidV1(cid.Raw, mh)
}

// provideLoop records this node as a provider of every model it serves, and
// refreshes the records often: they expire after 24 h, a refresh repairs a
// record lost when the DHT had no peers yet, and a bootstrap restart wipes
// the DHT, so a two minute refresh keeps cold clients able to find us.
func (n *Node) provideLoop(ctx context.Context) {
	every := 2 * time.Minute
	for {
		ok := true
		for _, m := range n.modelNames() {
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := n.dht.Provide(pctx, modelKey(m), true); err != nil {
				log.Printf("dht provide %s: %v", m, err)
				ok = false
			}
			cancel()
		}
		every = 2 * time.Minute
		if !ok { // the DHT was not ready; try again soon
			every = 15 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// findModelPeers asks the DHT who serves model and connects to them. It is
// the cold-start path: gossip may not have reached this node yet, and a model
// name is enough to find its servers.
func (n *Node) findModelPeers(ctx context.Context, model string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for ai := range n.dht.FindProvidersAsync(ctx, modelKey(model), 20) {
		if ai.ID == n.host.ID() {
			continue
		}
		n.addRelayPaths(ai.ID)
		if err := n.host.Connect(ctx, ai); err != nil {
			continue
		}
		n.hello(ctx, ai.ID)
	}
}

// hello asks a peer what it serves right now, so a provider found in the DHT
// is usable at once instead of after its next announce.
func (n *Node) hello(ctx context.Context, p peer.ID) {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := n.host.NewStream(hctx, p, protocol.HelloProto)
	if err != nil {
		return
	}
	defer s.Close()
	// The context only bounds the dial; without a read deadline a provider
	// that accepts the stream and says nothing blocks the caller for long
	// past the five seconds promised above.
	s.SetReadDeadline(time.Now().Add(5 * time.Second))
	var a protocol.Announce
	if err := json.NewDecoder(io.LimitReader(s, 1<<14)).Decode(&a); err != nil {
		return
	}
	if len(a.Models) > 0 && protocol.ValidAnnounce(&a) {
		n.remember(p, a)
	}
}

// handleHello answers with the announce this node last gossiped, or an empty
// one if it serves nothing.
func (n *Node) handleHello(s network.Stream) {
	defer s.Close()
	s.SetWriteDeadline(time.Now().Add(5 * time.Second))
	a := protocol.Announce{Version: Version, Models: []string{}}
	if last := n.lastAnn.Load(); last != nil {
		a = *last
	}
	b, _ := json.Marshal(a)
	s.Write(b)
}

// redialLoop reconnects to any bootstrap we lost. Without it a restarted
// bootstrap leaves nodes without a relay reservation until autorelay's
// backoff happens to retry, which took minutes.
func (n *Node) redialLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.cfg.RedialEvery):
		}
		for _, ai := range n.relays {
			if n.host.Network().Connectedness(ai.ID) == network.Connected {
				continue
			}
			// A refused dial during the restart must not push the next
			// one out by minutes.
			n.clearBackoff(ai.ID)
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := n.host.Connect(c, ai); err != nil {
				log.Printf("bootstrap %s: %v", ai.ID, err)
			} else {
				log.Printf("bootstrap %s: reconnected", ai.ID)
			}
			cancel()
		}
	}
}

// alive returns announces seen within three announce periods.
func (n *Node) alive() map[peer.ID]protocol.Announce {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[peer.ID]protocol.Announce{}
	for p, pi := range n.peers {
		if time.Since(pi.seen) < 3*n.cfg.AnnounceEvery+5*time.Second {
			out[p] = pi.Announce
		} else {
			delete(n.peers, p)
		}
	}
	return out
}

// candidates lists remote peers serving model, best first.
func (n *Node) candidates(model string) []peer.ID {
	type c struct {
		id      peer.ID
		free    int
		q       int
		rtt     time.Duration
		demoted bool // an audit suspect: used only when nobody else can answer
	}
	var cs []c
	want := models.Manifest[model].Weights
	for p, a := range n.alive() {
		// A peer that says it loads other weights than the name stands for
		// is serving another model under this name, by its own account.
		if h := a.Weights[model]; want != "" && h != "" && h != want {
			continue
		}
		var demoted bool
		if n.audits != nil {
			var out bool
			if out, demoted = n.audits.rank(p, model); out {
				continue
			}
		}
		for _, m := range a.Models {
			if m == model {
				// A peer we never dialed has no measured latency. Treat it as
				// last among equals, or every request would prefer a
				// stranger over the peer that just answered it.
				rtt := n.host.Peerstore().LatencyEWMA(p)
				if rtt <= 0 {
					rtt = time.Duration(1<<62 - 1)
				}
				cs = append(cs, c{p, a.Free, a.Q, rtt, demoted})
				break
			}
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].demoted != cs[j].demoted {
			return !cs[i].demoted
		}
		if (cs[i].free > 0) != (cs[j].free > 0) {
			return cs[i].free > 0
		}
		// A peer with a shorter queue sends the answer sooner than one with
		// a long one, whatever the ping says. q is the depth it announced.
		if cs[i].q != cs[j].q {
			return cs[i].q < cs[j].q
		}
		return cs[i].rtt < cs[j].rtt
	})
	out := make([]peer.ID, len(cs))
	for i := range cs {
		out[i] = cs[i].id
	}
	return out
}

// addRelayPaths records a route to p through the relays we know. autorelay
// only advertises circuit addresses built from a relay's public addresses, and
// a peer we only heard of over gossip may have no address at all, so we build
// the routes ourselves. Direct addresses, when known, are still dialed first,
// and hole punching upgrades a relayed connection when it can.
func (n *Node) addRelayPaths(p peer.ID) {
	for _, r := range n.relayInfos(4) {
		if r.ID == p {
			continue
		}
		circuit := ma.StringCast("/p2p/" + r.ID.String() + "/p2p-circuit")
		for _, a := range r.Addrs {
			n.host.Peerstore().AddAddr(p, a.Encapsulate(circuit), peerstore.TempAddrTTL)
		}
	}
}

// relaySource is autorelay's candidate source: the seeds plus public peers
// that announced relay=true. One seed is always included so a fresh node can
// join; public candidates are shuffled so reservations spread over them
// instead of all landing on the seed.
func (n *Node) relaySource(_ context.Context, num int) <-chan peer.AddrInfo {
	if num <= 0 {
		ch := make(chan peer.AddrInfo)
		close(ch)
		return ch
	}
	pubs := n.publicRelays()
	rand.Shuffle(len(pubs), func(i, j int) { pubs[i], pubs[j] = pubs[j], pubs[i] })
	var order []peer.AddrInfo
	if len(n.relays) > 0 {
		order = append(order, n.relays[0])
	}
	if len(pubs) > 0 {
		order = append(order, pubs[0])
	}
	if len(n.relays) > 1 {
		order = append(order, n.relays[1:]...)
	}
	if len(pubs) > 1 {
		order = append(order, pubs[1:]...)
	}
	ch := make(chan peer.AddrInfo, min(num, len(order)))
	seen := map[peer.ID]bool{}
	for _, ai := range order {
		if len(ch) == cap(ch) {
			break
		}
		if ai.ID == n.self || seen[ai.ID] || len(ai.Addrs) == 0 {
			continue
		}
		seen[ai.ID] = true
		ch <- ai
	}
	close(ch)
	return ch
}

// publicRelays returns recently seen peers that announced relay=true and whose
// addresses we know. Stale entries are pruned here too, like alive().
func (n *Node) publicRelays() []peer.AddrInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	ttl := 3*n.cfg.AnnounceEvery + 5*time.Second
	var out []peer.AddrInfo
	for p, pi := range n.peers {
		if time.Since(pi.seen) >= ttl {
			delete(n.peers, p)
			continue
		}
		if !pi.Relay || len(pi.addrs) == 0 {
			continue
		}
		out = append(out, peer.AddrInfo{ID: p, Addrs: append([]ma.Multiaddr(nil), pi.addrs...)})
	}
	return out
}

// relayInfos lists the relays we can build circuit addresses through: the
// seeds first, then public peers, capped so one peer's address list does not
// explode. Used for peers found through the DHT.
func (n *Node) relayInfos(limit int) []peer.AddrInfo {
	all := append([]peer.AddrInfo(nil), n.relays...)
	all = append(all, n.publicRelays()...)
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	out := all[:0]
	seen := map[peer.ID]bool{}
	for _, ai := range all {
		if ai.ID == n.self || seen[ai.ID] || len(ai.Addrs) == 0 {
			continue
		}
		seen[ai.ID] = true
		out = append(out, ai)
		if len(out) == limit {
			break
		}
	}
	return out
}

// discoverModels reads the upstream's model list. A server that hangs must not
// hold up the caller, which is New at startup and announceLoop after that.
func (n *Node) discoverModels(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, n.cfg.ModelsTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", n.cfg.Upstream+"/models", nil)
	n.upstreamAuth(req)
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET /models: %s", resp.Status)
	}
	var body struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelList)).Decode(&body); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range body.Data {
		out[m.ID] = m.ID
	}
	return out, nil
}

func (n *Node) upstreamAuth(req *http.Request) {
	if n.cfg.UpstreamKey != "" {
		req.Header.Set("Authorization", "Bearer "+n.cfg.UpstreamKey)
	}
}

// mapKeys returns the sorted keys of a public-name -> upstream-name map.
func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ID is this node's peer id.
func (n *Node) ID() string { return n.host.ID().String() }

// Idle reports whether the node is serving nothing right now: no running job,
// nobody in the queue, and no local client waiting on the gateway. cmd/bitllm
// restarts for an update only when this is true, so a release is never
// installed in the middle of an answer.
func (n *Node) Idle() bool {
	return n.inflight.Load() == 0 && n.queueDepth() == 0 && n.gatewayLive.Load() == 0
}

// Addrs is where peers can reach this node, with its peer id appended.
func (n *Node) Addrs() []string {
	var out []string
	for _, a := range n.host.Addrs() {
		out = append(out, a.Encapsulate(ma.StringCast("/p2p/"+n.host.ID().String())).String())
	}
	return out
}

// Seeds is the resolved bootstrap list: the peers this node dials first. It is
// empty when no --bootstrap was given or when the DNS seed list did not
// resolve, which is what lets cmd/bitllm print a join code instead.
func (n *Node) Seeds() []peer.AddrInfo {
	return append([]peer.AddrInfo(nil), n.relays...)
}

// ListenAddrs is where the p2p host accepts connections: TCP and QUIC on every
// IPv4 address, and on every IPv6 address unless ipv6 is off.
func ListenAddrs(port int, ipv6 bool) []string {
	addrs := []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
	}
	if ipv6 {
		addrs = append(addrs,
			fmt.Sprintf("/ip6/::/tcp/%d", port),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port))
	}
	return addrs
}
