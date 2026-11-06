// Package node is one BitLLM node: the libp2p host, the DHT and gossip
// topics, the serving queue, the local OpenAI/Anthropic gateway, signed
// receipts, membership and auditing. cmd/bitllm builds one and serves it.
package node

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/libp2p/go-libp2p/core/event"
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
	"github.com/pbnjay/memory"
)

const (
	// seedConnectTimeout bounds the first attempt to reach one seed.
	seedConnectTimeout = 10 * time.Second
	// syncEvery is how often a node syncs from a seed it already synced from.
	syncEvery = 10 * time.Minute
	// maxModelList caps the upstream's answer to GET /models.
	maxModelList = 4 << 20
	// provideRetry is how soon to provide again after a failure.
	provideRetry = 15 * time.Second
	// findConcurrency is how many providers one lookup connects to at a time.
	findConcurrency = 5
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
	ProvideEvery     time.Duration         // how often to refresh provider records with no other reason to; 0 = 30m
	SyncStall        time.Duration         // longest a seed may stay silent during a sync; 0 = 30s
	SyncLimit        time.Duration         // longest one sync from a seed may run; 0 = 5m
	ModelsTimeout    time.Duration         // longest one read of the upstream's model list may take; 0 = 10s
	UpstreamTimeout  time.Duration         // longest our server may take to send response headers; 0 = 10m
	PeerFields       []string              // request fields a peer may send beyond the standard ones (--peer-field)
	Weights          map[string]string     // public name -> bitllm hash-model of the files our server loads (announced, not proven)
	Audit            bool                  // spot-check the peers that answer us (audit.go)
	AuditEvery       int                   // real requests to a peer between its spot checks
	Auditors         []peer.ID             // auditors tried first
	AuditServe       bool                  // announce the models our server can score, so others may use us as an auditor
	Location         string                // coarse geohash cell for the node map (network.go); empty = not shown
	UpdateStatus     func() map[string]any // what the updater reports, for GET /status; nil = no updater
	PeerFallback     bool                  // while our own server is down, send requests for its models to peers
}

// peerInfo is one peer's latest announce and when we heard it. The announce
// type lives in the protocol package so a reader can find the whole wire
// contract in one place.
type peerInfo struct {
	protocol.Announce
	seen  time.Time
	addrs []ma.Multiaddr // where we can dial this peer as a relay; see relayAddrs
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
	servedRecent map[string]bool // cached, see contributors
	recentAt     time.Time
	syncGate     syncGate
	syncCursors  *syncCursors
	annRate      originRate // announce gossip, per origin
	claimRate    originRate // claim gossip, per origin
	receiptSeed  []byte
	self         peer.ID
	relayMu      sync.RWMutex
	relays       []peer.AddrInfo                // bootstrap nodes, which also relay; replaced whole, read through seeds
	seedStops    map[peer.ID]context.CancelFunc // what startSeed is running, per seed
	seedValues   map[string][]peer.AddrInfo     // what each --bootstrap value last gave; only refreshSeeds changes it after New
	http         *http.Client
	timers       serveTimers
	gate         requestGate
	modelMu      sync.RWMutex
	models       map[string]string // public name -> upstream name; replaced whole, never mutated
	inflight     atomic.Int32
	gatewayLive  atomic.Int32                      // completions the local gateway is answering, own model or proxied
	upOK         atomic.Bool                       // upstream answered its last health check
	lastAnn      atomic.Pointer[protocol.Announce] // what announceLoop last gossiped (GET /network counts us too)
	announceErr  atomic.Pointer[string]            // why our announce is short or not going out, for GET /status
	openInflight atomic.Int32                      // slots held by uninvited nodes on an open server
	imgMu        sync.Mutex
	imgLimit     map[string]int // upstream model -> most images its server takes, learned from a refusal
	audits       *audits        // nil without --audit
	peerMem      peerMemory     // how peers have done per model, for candidates

	mu    sync.Mutex
	peers map[peer.ID]*peerInfo

	provideKick chan struct{} // asks provideLoop for a pass now
	provides    atomic.Int64  // provider records sent, for tests

	limits     *publicLimits   // in force on a public node, nil on any other
	ctx        context.Context // the node's lifetime, for work started outside any loop
	relayFinds relayLookups    // DHT lookups of announced relays

	// qmu guards the serving slots and the queue: inflight, openInflight,
	// peerSlots and waiters are changed under it (queue.go).
	qmu       sync.Mutex
	peerSlots map[peer.ID]int32
	waiters   []*waiter
	svc       atomic.Int64 // average job duration in ns, for the queue's ETA
	refused   refusals     // why this server refused requests, for /status

	started time.Time // when this node came up, shown by GET /status

	// Stopping (shutdown.go).
	closing   atomic.Bool        // set first on shutdown: no new peer jobs
	closers   []func()           // run by Close once the host is down
	stopLoops context.CancelFunc // ends the loops that announce, dial and sync
	stopAll   context.CancelFunc // ends the rest: gossip, its readers, the DHT's context
	closeOnce sync.Once
	closeErr  error
}

// LoadKey reads the node key, creating it only when the file does not exist.
// The key is the node's identity, so a file that cannot be read or parsed is an
// error for a person to look at, and is never replaced.
func LoadKey(path string) (crypto.PrivKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		k, err := crypto.UnmarshalPrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("identity file %s is damaged (%w): restore it from a backup, or move it away to start with a new peer id", path, err)
		}
		return k, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot read identity file %s (%w): fix the file or its permissions; it has not been replaced", path, err)
	}
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, err
	}
	b, err = crypto.MarshalPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, b); err != nil {
		return nil, err
	}
	return k, nil
}

// writeFileAtomic writes a new private file in one step: a crash leaves either
// no file or the whole file, never part of one.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly once renamed
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// New starts a node: key, host, DHT, gossip, storage, then the loops. It does
// not wait for the network: the seeds are contacted in the background, so a
// seed that does not answer holds up neither the gateway nor the updater.
func New(ctx context.Context, cfg Config) (*Node, error) {
	if cfg.RedialEvery <= 0 {
		cfg.RedialEvery = 15 * time.Second
	}
	if cfg.AnnounceEvery <= 0 {
		cfg.AnnounceEvery = 10 * time.Second
	}
	if cfg.ProvideEvery <= 0 {
		cfg.ProvideEvery = 30 * time.Minute
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
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 10 * time.Minute
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
	seedValues, bootstrap, err := resolveSeeds(ctx, cfg.Bootstrap, nil)
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
	// Gossip outlives loops: a job that finishes during a drain still publishes
	// its receipt, while nothing announces this node any more.
	ctx, stopAll := context.WithCancel(ctx)
	loops, stopLoops := context.WithCancel(ctx)
	n := &Node{
		stopAll:     stopAll,
		stopLoops:   stopLoops,
		cfg:         cfg,
		ledger:      ledger.Open(filepath.Join(cfg.DataDir, "ledger.json")),
		http:        upstreamClient(cfg.UpstreamTimeout),
		timers:      newServeTimers(),
		gate:        requestGate{budget: bodyBudget},
		peers:       map[peer.ID]*peerInfo{},
		peerSlots:   map[peer.ID]int32{},
		ctx:         loops,
		provideKick: make(chan struct{}, 1),
		self:        self,
		relays:      bootstrap,
		seedValues:  seedValues,
		models:      cfg.Models,
		started:     time.Now(),
	}
	// A caller that supplied a map (--model, or a test) pinned it. Only a
	// node that discovered its models keeps re-reading them from upstream.
	n.cfg.ModelsFixed = n.cfg.ModelsFixed || len(cfg.Models) > 0
	mode := dht.ModeAutoServer
	reservations := defaultReservations
	if public {
		mode = dht.ModeServer
		limits := limitsForMemory(memory.TotalMemory())
		n.limits, reservations = &limits, limits.Reservations
	}
	opts := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(),
		// Public nodes relay for NATed ones until hole punching upgrades the link.
		// Inference streams are long, so the limits are far above one answer.
		libp2p.EnableRelayService(relay.WithResources(relayResources(cfg.RelayBehindProxy, reservations))),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			var err error
			n.dht, err = dht.New(h, dht.Mode(mode), dht.ProtocolPrefix("/bitllm"),
				dht.BootstrapPeersFunc(n.seeds))
			return n.dht, err
		}),
	}
	if n.limits != nil {
		opts = append(opts, n.limits.options()...)
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
	}

	n.nodeKey = key
	if n.receiptSeed, err = receipts.LoadSeed(filepath.Join(cfg.DataDir, "receipt.seed")); err != nil {
		// Losing the seed only breaks the link to past pseudonyms, which is no
		// reason to stay down. Use one for this run and leave the file alone.
		log.Printf("WARNING: %v; using a temporary one until the next start", err)
		n.receiptSeed = make([]byte, 32)
		if _, err := crand.Read(n.receiptSeed); err != nil {
			return nil, err
		}
	}
	n.claims = account.OpenClaims(filepath.Join(cfg.DataDir, "claims.json"))
	n.quota = quota.Open(filepath.Join(cfg.DataDir, "quota.json"))
	if n.receipts, err = receipts.Open(filepath.Join(cfg.DataDir, "receipts.jsonl"), key, n.host.ID(), n.receiptSeed); err != nil {
		return nil, err
	}
	// After receipts and claims: replaying saved invites needs to know who served.
	n.members = members.Open(filepath.Join(cfg.DataDir, "members.json"), cfg.Roots, n.servedSets)
	n.wireState()

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
	go n.claimLoop(loops)
	if n.topic, err = ps.Join(protocol.AnnounceTopic); err != nil {
		return nil, err
	}
	sub, err := n.topic.Subscribe()
	if err != nil {
		return nil, err
	}
	go n.readAnnounces(ctx, sub)

	// The host is already listening, so handlers go up only now that everything
	// they use exists.
	n.host.SetStreamHandler(protocol.ReceiptProto, n.handleReceipt)
	n.host.SetStreamHandler(protocol.SyncProto, n.handleSync)
	n.host.SetStreamHandler(protocol.SyncProto2, n.handleSync2)
	n.host.SetStreamHandler(protocol.HelloProto, n.handleHello)
	if cfg.Upstream != "" {
		n.host.SetStreamHandler(protocol.InferProto, n.handleInfer)
		n.host.SetStreamHandler(protocol.InferProto2, n.handleInfer)
	}

	if err := n.dht.Bootstrap(ctx); err != nil {
		return nil, err
	}
	if cfg.MDNS {
		mdns.NewMdnsService(n.host, protocol.Rendezvous, n).Start()
	}
	if cfg.Audit {
		n.audits = newAudits(n, cfg.AuditEvery, cfg.Auditors)
		go n.audits.recheckLoop(loops)
	}
	n.closers = append(n.closers, n.flushState)
	go n.joinSeeds(loops)
	go n.redialLoop(loops)
	if len(cfg.Bootstrap) > 0 {
		go n.reseedLoop(loops)
	}
	if cfg.Upstream != "" {
		n.upOK.Store(n.checkUpstream(ctx))
	}
	if cfg.Upstream != "" || public {
		go n.announceLoop(loops)
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

// A relayed connection may run this long and carry this much in each direction.
// Far above any one answer, so long answers finish, and a relay stays bounded.
// Over the limit the relay resets it and the next request opens another.
const (
	relayCircuitDuration = 2 * time.Hour
	relayCircuitData     = 1 << 30
)

// relayResources: bounded circuits and slots, so a public bootstrap is not an
// open-ended relay.
//
// Any limit marks a relayed connection as limited on both ends, and libp2p then
// opens a stream on it only if the caller allows that. Inference, receipts, sync
// (in every release), hello, identify and hole punching do. Gossip and the DHT do
// not, so they use direct connections: every node has one to its seed, and only
// public nodes are DHT servers. A stream opened without the option waits for a
// direct connection, which is what hello does on releases from v0.3.0 to v0.4.6;
// those clients then wait for the next announce instead.
//
// The default cap of 8 reservations per source IP is the only sybil guard on a
// public-IP relay, but behind a tunnel every peer arrives from 127.0.0.1, so
// there it would let only 8 NATed nodes in the whole network be reachable.
func relayResources(behindProxy bool, reservations int) relay.Resources {
	rc := relay.DefaultResources()
	rc.Limit = &relay.RelayLimit{Duration: relayCircuitDuration, Data: relayCircuitData}
	rc.MaxReservations = reservations // NATed peers that can be reached through us
	rc.MaxCircuits = 64               // open relayed connections per peer
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
	for _, ai := range n.seeds() {
		n.startSeed(ctx, ai)
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
// the first check that passes. A public node keeps announcing relay=true with
// no models, with or without an upstream, so NATed nodes can use it as a relay.
func (n *Node) announceLoop(ctx context.Context) {
	var scorable []string
	probed := false
	lastModels := time.Time{}
	var reported announceLog
	omitted := 0 // kept while the upstream is down, when there is no list to count
	for {
		// A job in flight already proves the server works, and a server that
		// handles one request at a time may not answer the probe meanwhile.
		if n.cfg.Upstream != "" && n.inflight.Load() == 0 {
			ok := n.checkUpstream(ctx)
			if ctx.Err() != nil {
				return // stopping: a check cut short says nothing about the server
			}
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
		free, slots, clamped := announcedSlots(n.cfg.Concurrency, int(n.inflight.Load()))
		a := protocol.Announce{Version: Version, Models: []string{}, Free: free, Max: slots, Q: n.queueDepth(), Relay: n.cfg.DHTServer || n.cfg.Public}
		if n.upOK.Load() {
			a.Models = n.modelNames()
			if n.cfg.AuditServe && !probed {
				scorable, probed = n.probeScoring(ctx), true
				if len(scorable) > 0 {
					log.Printf("our server can score %v: announcing it, so peers may use this node as an auditor", scorable)
				}
			}
			a.Audit = scorable
			a.Weights = n.cfg.Weights
			a.Loc = n.cfg.Location
			omitted = capAnnounce(&a)
		}
		if len(a.Models) == 0 {
			a.Free, a.Max = 0, 1
		}
		if !slices.Equal(n.announced(), a.Models) || n.lastAnn.Load() == nil {
			n.kickProvide()
		}
		n.lastAnn.Store(&a)
		problems := announceProblems{omitted: omitted, clamped: clamped && n.cfg.Upstream != ""}
		// A node that serves nothing and is no relay has nothing valid to send,
		// and a stopped model server is not an announce problem.
		if len(a.Models) > 0 || a.Relay {
			b, _ := json.Marshal(a)
			if err := n.topic.Publish(ctx, b); err != nil && ctx.Err() == nil {
				problems.publish = err.Error()
			}
		}
		reported.report(problems, slots)
		n.announceErr.Store(new(problems.String()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(n.cfg.AnnounceEvery):
		}
	}
}

// capAnnounce fits a to what peers accept and returns how many models it left
// out. Peers reject an announce whole if it lists too many models, an invalid
// name, or a model in Audit or Weights that it does not list, which would hide
// the node entirely. It keeps valid names, canonical ones first and then sorted,
// so the choice is stable. Only the announce is capped: a local client can still
// ask its own gateway for any model.
func capAnnounce(a *protocol.Announce) (omitted int) {
	var canonical, other []string
	for _, m := range a.Models {
		if !protocol.ValidModelName(m) {
			omitted++
		} else if _, ok := models.Manifest[m]; ok {
			canonical = append(canonical, m)
		} else {
			other = append(other, m)
		}
	}
	slices.Sort(canonical)
	slices.Sort(other)
	a.Models = append(append([]string{}, canonical...), other...)
	if len(a.Models) > protocol.MaxAnnounceModels {
		omitted += len(a.Models) - protocol.MaxAnnounceModels
		a.Models = a.Models[:protocol.MaxAnnounceModels]
	}
	kept := make(map[string]bool, len(a.Models))
	for _, m := range a.Models {
		kept[m] = true
	}
	a.Audit = slices.DeleteFunc(slices.Clone(a.Audit), func(m string) bool { return !kept[m] })
	if a.Weights != nil {
		weights := make(map[string]string, len(a.Weights))
		for m, h := range a.Weights {
			if kept[m] && models.ValidHash(h) {
				weights[m] = h
			}
		}
		a.Weights = weights
	}
	return omitted
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
	var addrs []ma.Multiaddr
	if n.host != nil && a.Relay {
		addrs = relayAddrs(n.host.Peerstore().Addrs(from), n.onLocalNetwork())
	}
	n.mu.Lock()
	old, known := n.peers[from]
	if !known && len(n.peers) >= maxPeers {
		oldest, at := peer.ID(""), time.Now()
		for p, pi := range n.peers {
			if pi.seen.Before(at) {
				oldest, at = p, pi.seen
			}
		}
		delete(n.peers, oldest)
	}
	if len(addrs) == 0 && known && a.Relay {
		addrs = old.addrs // what a lookup found outlives the peerstore entry
	}
	n.peers[from] = &peerInfo{Announce: a, seen: time.Now(), addrs: addrs}
	n.mu.Unlock()
	if a.Relay && len(addrs) == 0 {
		n.lookupRelay(from) // gossip carries no addresses
	}
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

// provideLoop records this node as a provider of every model it announces, so
// a cold client can find it by model name. A record stays valid for 48 h and the
// address in it for 24 h, so the periodic refresh is only a repair. What matters
// is the moments a record goes missing or wrong: at start, when the announced
// models change, when a seed connects, since a seed that restarted has lost its
// records, and when our own addresses change, since a NATed node has none until
// it holds a relay reservation and a record with no address is not kept. Nothing
// is announced while the upstream is down, so that stops the records too, and
// recovery brings them back.
func (n *Node) provideLoop(ctx context.Context) {
	go n.watchProvideTriggers(ctx)
	for {
		// A request from before this point is covered by this pass.
		select {
		case <-n.provideKick:
		default:
		}
		wait := n.cfg.ProvideEvery
		if !n.provideAll(ctx) { // the DHT was not ready; try again soon
			wait = provideRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-n.provideKick:
		case <-time.After(wait):
		}
	}
}

// provideAll sends one provider record per announced model and reports whether
// they all went out. With no address of our own yet, none would be kept.
func (n *Node) provideAll(ctx context.Context) bool {
	announced := n.announced()
	if len(announced) > 0 && len(n.host.Addrs()) == 0 {
		return false
	}
	ok := true
	for _, m := range announced {
		if !n.upOK.Load() {
			break
		}
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if err := n.dht.Provide(pctx, modelKey(m), true); err != nil {
			log.Printf("dht provide %s: %v", m, err)
			ok = false
		} else {
			n.provides.Add(1)
		}
		cancel()
	}
	return ok
}

// kickProvide asks provideLoop for a pass as soon as it can.
func (n *Node) kickProvide() {
	select {
	case n.provideKick <- struct{}{}:
	default:
	}
}

// watchProvideTriggers asks for a provide pass when our addresses change, and
// when a seed connects, once the seed is in the routing table, where the pass
// can reach it.
func (n *Node) watchProvideTriggers(ctx context.Context) {
	sub, err := n.host.EventBus().Subscribe([]any{new(event.EvtPeerConnectednessChanged), new(event.EvtLocalAddressesUpdated)})
	if err != nil {
		log.Printf("watching for provide triggers: %v", err)
		return
	}
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub.Out():
			if !ok {
				return
			}
			switch ev := e.(type) {
			case event.EvtLocalAddressesUpdated:
				n.kickProvide()
			case event.EvtPeerConnectednessChanged:
				if ev.Connectedness != network.Connected || !n.isSeed(ev.Peer) {
					continue
				}
				go func() {
					for deadline := time.Now().Add(seedConnectTimeout); n.dht.RoutingTable().Find(ev.Peer) == "" && time.Now().Before(deadline); {
						select {
						case <-ctx.Done():
							return
						case <-time.After(50 * time.Millisecond):
						}
					}
					n.kickProvide()
				}()
			}
		}
	}
}

// findModelPeers asks the DHT who serves model and connects to them. It is
// the cold-start path: gossip may not have reached this node yet, and a model
// name is enough to find its servers.
func (n *Node) findModelPeers(ctx context.Context, model string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n.connectProviders(ctx, n.dht.FindProvidersAsync(ctx, modelKey(model), 20))
}

// connectProviders connects to the providers in found and asks each what it
// serves, findConcurrency at a time, so a dead provider does not spend the whole
// time budget of the ones behind it.
func (n *Node) connectProviders(ctx context.Context, found <-chan peer.AddrInfo) {
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, findConcurrency)
	for ai := range found {
		if ai.ID == n.host.ID() {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case slots <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-slots }()
			n.addRelayPaths(ai.ID)
			if err := n.host.Connect(ctx, ai); err != nil {
				return
			}
			n.hello(ctx, ai.ID)
		})
	}
}

// hello asks a peer what it serves right now, so a provider found in the DHT
// is usable at once instead of after its next announce.
func (n *Node) hello(ctx context.Context, p peer.ID) {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The provider is often NATed, so this may be a limited relayed connection.
	s, err := n.host.NewStream(network.WithAllowLimitedConn(hctx, "hello"), p, protocol.HelloProto)
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
	if protocol.ValidAnnounce(&a) {
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
		for _, ai := range n.seeds() {
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

// rttNear is how close two latencies must be to count as equal when ranking.
const rttNear = 50 * time.Millisecond

// candidates lists remote peers serving model, best first: a peer in cooldown
// goes behind all others but is still listed, so a sole server gets retried.
// Peers that rank the same come in random order, so load spreads over them.
func (n *Node) candidates(model string) []peer.ID {
	type c struct {
		id      peer.ID
		free    int
		q       int
		rtt     time.Duration
		demoted bool // an audit suspect: used only when nobody else can answer
		cooling bool // failed recently
		recent  bool // answered recently
	}
	var cs []c
	want := models.Manifest[model].Weights
	now := time.Now()
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
				cooling, recent := n.peerMem.rank(p, model, now)
				cs = append(cs, c{p, a.Free, a.Q, rtt, demoted, cooling, recent})
				break
			}
		}
	}
	rand.Shuffle(len(cs), func(i, j int) { cs[i], cs[j] = cs[j], cs[i] })
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].cooling != cs[j].cooling {
			return !cs[i].cooling
		}
		if cs[i].demoted != cs[j].demoted {
			return !cs[i].demoted
		}
		if (cs[i].free > 0) != (cs[j].free > 0) {
			return cs[i].free > 0
		}
		if cs[i].recent != cs[j].recent {
			return cs[i].recent
		}
		// A peer with a shorter queue sends the answer sooner than one with
		// a long one, whatever the ping says. q is the depth it announced.
		if cs[i].q != cs[j].q {
			return cs[i].q < cs[j].q
		}
		return cs[i].rtt/rttNear < cs[j].rtt/rttNear
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
	seeds := n.seeds()
	var order []peer.AddrInfo
	if len(seeds) > 0 {
		order = append(order, seeds[0])
	}
	if len(pubs) > 0 {
		order = append(order, pubs[0])
	}
	if len(seeds) > 1 {
		order = append(order, seeds[1:]...)
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
	all := append([]peer.AddrInfo(nil), n.seeds()...)
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

// SeedConnected reports whether this node is connected to any of its seeds.
func (n *Node) SeedConnected() bool {
	for _, ai := range n.relays {
		if n.host.Network().Connectedness(ai.ID) == network.Connected {
			return true
		}
	}
	return false
}

// Seeds is the resolved bootstrap list: the peers this node dials first. It is
// empty when no --bootstrap was given or when the DNS seed list did not
// resolve, which is what lets cmd/bitllm print a join code instead.
func (n *Node) Seeds() []peer.AddrInfo {
	return append([]peer.AddrInfo(nil), n.seeds()...)
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
