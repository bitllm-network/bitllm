package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// rawAnnouncer is a libp2p host that publishes on the announce topic with none
// of a node's own checks, the way any other implementation could.
func rawAnnouncer(t *testing.T, ctx context.Context, to *Node) (host.Host, *pubsub.Topic) {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	topic, err := ps.Join(protocol.AnnounceTopic)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Connect(ctx, peer.AddrInfo{ID: to.host.ID(), Addrs: to.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	return h, topic
}

func publishAnnounce(ctx context.Context, topic *pubsub.Topic, a protocol.Announce) error {
	b, _ := json.Marshal(a)
	return topic.Publish(ctx, b)
}

func modelList(t *testing.T, n *Node) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	n.Gateway().ServeHTTP(w, gwReq("GET", "/v1/models", nil))
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Data
}

// TestGossipDropsBadModelName: an announce with a name outside the allow-list
// is neither stored nor passed on. A clean announce sent the same way shows
// that the path works.
func TestGossipDropsBadModelName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := testNode(t, ctx, Config{})
	second := testNode(t, ctx, Config{Bootstrap: first.Addrs()[:1]})
	bad, badTopic := rawAnnouncer(t, ctx, first)
	good, goodTopic := rawAnnouncer(t, ctx, first)

	deadline := time.Now().Add(20 * time.Second)
	for {
		publishAnnounce(ctx, badTopic, protocol.Announce{Models: []string{"bad name"}, Max: 1})
		publishAnnounce(ctx, goodTopic, protocol.Announce{Models: []string{"good-name"}, Max: 1})
		_, onFirst := first.alive()[good.ID()]
		_, onSecond := second.alive()[good.ID()]
		if onFirst && onSecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clean announce did not arrive: first=%v second=%v", onFirst, onSecond)
		}
		time.Sleep(300 * time.Millisecond)
	}
	for name, n := range map[string]*Node{"first": first, "second": second} {
		if a, ok := n.alive()[bad.ID()]; ok {
			t.Fatalf("%s stored %+v", name, a)
		}
	}
}

// TestHelloDropsBadModelName: the hello path applies the same check.
func TestHelloDropsBadModelName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testNode(t, ctx, Config{})
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetStreamHandler(protocol.HelloProto, func(s network.Stream) {
		defer s.Close()
		b, _ := json.Marshal(protocol.Announce{Models: []string{"bad name"}, Free: 1, Max: 1})
		s.Write(b)
	})
	if err := client.host.Connect(ctx, peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}); err != nil {
		t.Fatal(err)
	}
	client.hello(ctx, h.ID())
	if a, ok := client.alive()[h.ID()]; ok {
		t.Fatalf("hello stored %+v", a)
	}
}

// TestListModelsCanonical: /v1/models marks the names in models.json, keeps its
// other fields, and lists no invalid name even if one reached the table.
func TestListModelsCanonical(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := testNode(t, ctx, Config{})
	n.remember(randomPeer(t), protocol.Announce{Models: []string{"deepseek-v4.1-flash-1017", "my-lab/private", "bad name"}, Max: 1})

	got := map[string]map[string]any{}
	for _, m := range modelList(t, n) {
		got[m["id"].(string)] = m
	}
	if _, ok := got["bad name"]; ok || len(got) != 2 {
		t.Fatalf("model list = %v", got)
	}
	if got["deepseek-v4.1-flash-1017"]["canonical"] != true || got["my-lab/private"]["canonical"] != false {
		t.Fatalf("canonical flags wrong: %v", got)
	}
	m := got["my-lab/private"]
	if m["object"] != "model" || m["owned_by"] != "bitllm" || m["nodes"] != float64(1) {
		t.Fatalf("existing fields changed: %v", m)
	}
}

func statusOf(t *testing.T, n *Node) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	n.Gateway().ServeHTTP(w, gwReq("GET", "/status", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAnnouncedModels: valid names only, canonical first, then sorted, capped.
func TestAnnouncedModels(t *testing.T) {
	names := []string{"bad name", "deepseek-v4.1-flash-1017", "$m"}
	for i := range protocol.MaxAnnounceModels + 2 {
		names = append(names, fmt.Sprintf("m%02d", i))
	}
	kept, omitted := announcedModels(names)
	if len(kept) != protocol.MaxAnnounceModels || omitted != 5 {
		t.Fatalf("kept %d, omitted %d", len(kept), omitted)
	}
	if kept[0] != "deepseek-v4.1-flash-1017" || kept[1] != "m00" || !slices.IsSorted(kept[1:]) {
		t.Fatalf("order %v", kept)
	}
}

// TestLongModelListStaysVisible: a server with more models than an announce
// holds, a name that fails the check, a slot count out of range and weights for
// a model left out still reaches other nodes. It announces and provides only
// what fits, it still serves the rest to a local client, and /status says what
// happened.
func TestLongModelListStaysVisible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local")
	defer up.Close()
	served := map[string]string{"bad name": "local", "deepseek-v4.1-flash-1017": "local"}
	weights := map[string]string{"deepseek-v4.1-flash-1017": strings.Repeat("a", 64)}
	for i := range protocol.MaxAnnounceModels + 4 {
		m := fmt.Sprintf("m%02d", i)
		served[m] = "local"
		weights[m] = strings.Repeat("b", 64)
	}
	boot := testNode(t, ctx, Config{})
	server := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1], Upstream: up.URL + "/v1", Models: served,
		Weights: weights, Concurrency: protocol.MaxAnnounceSlots + 10})
	client := testNode(t, ctx, Config{Bootstrap: boot.Addrs()[:1]})

	var a protocol.Announce
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var ok bool
		if a, ok = client.alive()[server.host.ID()]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never reached the client")
		}
	}
	if len(a.Models) != protocol.MaxAnnounceModels || a.Models[0] != "deepseek-v4.1-flash-1017" {
		t.Fatalf("announced %v", a.Models)
	}
	if a.Max != protocol.MaxAnnounceSlots || a.Free != protocol.MaxAnnounceSlots {
		t.Fatalf("announced slots free=%d max=%d", a.Free, a.Max)
	}
	if len(a.Weights) != len(a.Models) {
		t.Fatalf("weights %d, models %d", len(a.Weights), len(a.Models))
	}
	e, _ := statusOf(t, server)["announce_error"].(string)
	if !strings.Contains(e, "6 models not announced") || !strings.Contains(e, "--concurrency") {
		t.Fatalf("announce_error = %q", e)
	}
	if _, ok := statusOf(t, client)["announce_error"]; ok {
		t.Fatal("an empty announce_error must be left out")
	}

	// Only announced models get provider records.
	providers := func(model string) (found bool) {
		fctx, fcancel := context.WithTimeout(ctx, 2*time.Second)
		defer fcancel()
		for ai := range client.dht.FindProvidersAsync(fctx, modelKey(model), 5) {
			found = found || ai.ID == server.host.ID()
		}
		return found
	}
	for deadline := time.Now().Add(20 * time.Second); !providers(a.Models[0]); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no provider record for an announced model")
		}
	}
	omitted := fmt.Sprintf("m%02d", protocol.MaxAnnounceModels+3)
	if providers(omitted) {
		t.Fatal("provider record for a model left out of the announce")
	}

	// A local client can still use a model that did not fit.
	w := post(server, fmt.Sprintf(`{"model":%q,"messages":[]}`, omitted))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello world") {
		t.Fatalf("local request: %d %s", w.Code, w.Body)
	}
}

// TestPublishFailureIsReported: an announce that cannot go out shows in /status.
func TestPublishFailureIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "local")
	defer up.Close()
	n := testNode(t, ctx, Config{Upstream: up.URL + "/v1", Models: map[string]string{"m": "local"},
		Location: strings.Repeat("u", protocol.MaxLocJSON+1)}) // fails the topic's own validator
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if e, _ := statusOf(t, n)["announce_error"].(string); strings.Contains(e, "publish failed") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no publish failure in /status")
		}
	}
}

// TestBarePublicNodeIsARelayCandidate: a --public node with no model server
// reaches a NATed node that never dialed it. The NATed node learns where to dial
// it, takes a reservation on it, and does not list it as serving anything.
func TestBarePublicNodeIsARelayCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seed := testNode(t, ctx, Config{})
	bs := seed.Addrs()[:1]
	natted, err := New(ctx, Config{Bootstrap: bs, Private: true, DataDir: t.TempDir(), AnnounceEvery: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { natted.host.Close() })
	// After the NATed node, so its first look for peers cannot find this one.
	time.Sleep(500 * time.Millisecond)
	bare, err := New(ctx, Config{Bootstrap: bs, Public: true, Listen: []string{"/ip4/127.0.0.1/tcp/0"},
		DataDir: t.TempDir(), AnnounceEvery: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bare.host.Close() })

	candidate := func() bool {
		for _, ai := range natted.publicRelays() {
			if ai.ID == bare.host.ID() && len(ai.Addrs) > 0 {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(15 * time.Second); !candidate(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			_, heard := natted.alive()[bare.host.ID()]
			t.Fatalf("no relay candidate; heard its announce: %v", heard)
		}
	}
	found := false
	for ai := range natted.relaySource(ctx, 8) {
		found = found || ai.ID == bare.host.ID()
	}
	if !found {
		t.Fatal("autorelay's source does not offer the bare node")
	}

	// A hop to the NATed node through the bare one works only with a reservation.
	circuit := ma.StringCast(bare.Addrs()[0] + "/p2p-circuit/p2p/" + natted.host.ID().String())
	probe := testNode(t, ctx, Config{})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		cctx, c := context.WithTimeout(ctx, 3*time.Second)
		err := probe.host.Connect(cctx, peer.AddrInfo{ID: natted.host.ID(), Addrs: []ma.Multiaddr{circuit}})
		c()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reservation on the bare node: %v", err)
		}
	}

	// It serves nothing, on every list a client reads.
	a, ok := natted.alive()[bare.host.ID()]
	if !ok || len(a.Models) != 0 || !a.Relay || a.Max != 1 || a.Free != 0 {
		t.Fatalf("announce = %+v, %v", a, ok)
	}
	if got := natted.candidates(""); len(got) != 0 {
		t.Fatalf("candidates = %v", got)
	}
	if got := modelList(t, natted); len(got) != 0 {
		t.Fatalf("/v1/models = %v", got)
	}
	if v := natted.networkSnapshot(); v.Nodes != 0 || len(v.Models) != 0 {
		t.Fatalf("/network = %+v", v)
	}
	if bare.ownAnnounce() != nil {
		t.Fatal("a relay-only node counted itself as serving")
	}
}

// TestHelloKeepsRelayAnnounce: hello accepts a relay-only announce, and still
// drops the empty answer of a node that serves nothing and is no relay.
func TestHelloKeepsRelayAnnounce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testNode(t, ctx, Config{})
	bare := testNode(t, ctx, Config{Bootstrap: client.Addrs()[:1]})
	plain, err := New(ctx, Config{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, DataDir: t.TempDir(), AnnounceEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.host.Close() })
	for bare.lastAnn.Load() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	for _, p := range []*Node{bare, plain} {
		if err := client.host.Connect(ctx, peer.AddrInfo{ID: p.host.ID(), Addrs: p.host.Addrs()}); err != nil {
			t.Fatal(err)
		}
		client.hello(ctx, p.host.ID())
	}
	alive := client.alive()
	if a, ok := alive[bare.host.ID()]; !ok || !a.Relay {
		t.Fatalf("relay announce not kept: %+v, %v", a, ok)
	}
	if a, ok := alive[plain.host.ID()]; ok {
		t.Fatalf("stored an empty answer: %+v", a)
	}
}

// TestRelayAddrs: only dialable public IP addresses count, at most eight, and
// private ones count only on a local network.
func TestRelayAddrs(t *testing.T) {
	addr := func(ss ...string) []ma.Multiaddr {
		var out []ma.Multiaddr
		for _, s := range ss {
			out = append(out, ma.StringCast(s))
		}
		return out
	}
	const circuit = "/ip4/8.8.8.8/tcp/4001/p2p/" + testSeedA + "/p2p-circuit"
	in := addr("/ip4/127.0.0.1/tcp/1", "/ip4/10.0.0.1/tcp/1", "/ip4/192.168.1.1/tcp/1", "/ip6/::1/tcp/1",
		"/dns4/example.com/tcp/1", circuit, "/ip4/8.8.8.8/tcp/1", "/ip4/8.8.8.8/udp/1/quic-v1", "/ip6/2001:4860:4860::8888/tcp/1")
	if got := relayAddrs(in, false); len(got) != 3 {
		t.Fatalf("public network kept %v", got)
	}
	if got := relayAddrs(in, true); len(got) != 7 {
		t.Fatalf("local network kept %v", got)
	}
	var many []string
	for i := range maxRelayAddrs + 4 {
		many = append(many, fmt.Sprintf("/ip4/8.8.8.8/tcp/%d", i+1))
	}
	if got := relayAddrs(addr(many...), false); len(got) != maxRelayAddrs {
		t.Fatalf("kept %d addresses", len(got))
	}
}

// TestRelayLookupLimits: one lookup at a time, a gap between lookups, and each
// peer at most once per period.
func TestRelayLookupLimits(t *testing.T) {
	var l relayLookups
	a, b := randomPeer(t), randomPeer(t)
	now := time.Now()
	if !l.allow(a, now) {
		t.Fatal("first lookup refused")
	}
	if l.allow(b, now.Add(relayLookupGap)) {
		t.Fatal("second lookup started during the first")
	}
	l.done()
	if l.allow(b, now.Add(relayLookupGap-time.Millisecond)) {
		t.Fatal("lookup started inside the gap")
	}
	if !l.allow(b, now.Add(relayLookupGap)) {
		t.Fatal("lookup of another peer refused after the gap")
	}
	l.done()
	if l.allow(a, now.Add(relayLookupPerPeer-time.Second)) {
		t.Fatal("same peer looked up twice in one period")
	}
	if !l.allow(a, now.Add(relayLookupPerPeer)) {
		t.Fatal("peer not looked up again after the period")
	}
}
