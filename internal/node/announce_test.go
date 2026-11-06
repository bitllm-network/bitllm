package node

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
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
