package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-network/bitllm/internal/protocol"
)

func TestGeohash(t *testing.T) {
	// Reference values from the geohash article on Wikipedia, and Berlin's
	// well-known cell.
	for _, c := range []struct {
		lat, lon float64
		want     string
	}{
		{57.64911, 10.40744, "u4pruydqqvj"},
		{42.605, -5.603, "ezs42"},
		{52.52, 13.40, "u33"},
	} {
		if got := geohash(c.lat, c.lon, len(c.want)); got != c.want {
			t.Errorf("geohash(%v,%v) = %s, want %s", c.lat, c.lon, got, c.want)
		}
	}
	lat, lon, ok := cellCenter("u33")
	if !ok || lat < 51 || lat > 54 || lon < 12 || lon > 15 {
		t.Fatalf("cellCenter(u33) = %v,%v,%v", lat, lon, ok)
	}
}

func TestParseLocation(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"none":               "",
		"52.52,13.40":        "u33",
		" 52.5201 , 13.4049": "u33",
		"u33dc0x":            "u33",
		"U33":                "u33",
	} {
		got, err := parseLocation(in)
		if err != nil || got != want {
			t.Errorf("parseLocation(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"berlin", "91,0", "0,181", "u3", "u3a", "1,2,3"} {
		if _, err := parseLocation(in); err == nil {
			t.Errorf("parseLocation(%q): want an error", in)
		}
	}
}

func TestPeerCell(t *testing.T) {
	for in, want := range map[string]string{
		"":         "",
		"u33":      "u33",
		"u33dc0x0": "u33", // a finer cell from some other version is cut down
		"u3":       "",
		"U33":      "", // not something we send; ignore rather than guess
		"<b>":      "",
		"u33 ":     "",
	} {
		if got := peerCell(in); got != want {
			t.Errorf("peerCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregate(t *testing.T) {
	v := aggregate([]protocol.Announce{
		{Version: "v0.2.1", Models: []string{"a", "b"}, Free: 1, Max: 2, Loc: "u33"},
		{Version: "v0.2.1", Models: []string{"a"}, Free: 0, Max: 1, Loc: "u33"},
		{Version: "v0.1.13", Models: []string{"a"}, Free: 4, Max: 4},
		{Models: []string{"b"}, Free: 1, Max: 1, Loc: "garbage!"},
		{Models: nil, Loc: "u33"}, // serves nothing right now: not on the map
	}, time.Unix(0, 0))
	if v.Nodes != 4 || v.Unlocated != 2 {
		t.Fatalf("nodes %d unlocated %d", v.Nodes, v.Unlocated)
	}
	if len(v.Models) != 2 || v.Models[0] != (networkModel{ID: "a", Nodes: 3, Free: 5, Max: 7}) ||
		v.Models[1] != (networkModel{ID: "b", Nodes: 2, Free: 2, Max: 3}) {
		t.Fatalf("models %+v", v.Models)
	}
	if len(v.Cells) != 1 || v.Cells[0].Cell != "u33" || v.Cells[0].Nodes != 2 ||
		v.Cells[0].Models["a"] != 2 || v.Cells[0].Models["b"] != 1 {
		t.Fatalf("cells %+v", v.Cells)
	}
	if v.Versions["v0.2.1"] != 2 || v.Versions["v0.1.13"] != 1 || v.Versions["unknown"] != 1 {
		t.Fatalf("versions %v", v.Versions)
	}
}

func TestAnnounceLocLimit(t *testing.T) {
	a := protocol.Announce{Models: []string{"m"}, Free: 1, Max: 1, Loc: strings.Repeat("u", protocol.MaxLocJSON)}
	if !protocol.ValidAnnounce(&a) {
		t.Fatal("a loc of protocol.MaxLocJSON bytes must pass")
	}
	a.Loc += "u"
	if protocol.ValidAnnounce(&a) {
		t.Fatal("a longer loc must be refused")
	}
}

// A server with --location shows up on a client's /network as a cell, with no
// peer id and no finer position anywhere in the answer.
func TestNetworkMap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "m")
	defer up.Close()

	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	loc, _ := parseLocation("52.5201,13.4049")
	server := testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1", Location: loc})
	client := testNode(t, ctx, Config{Bootstrap: bs})

	var v networkView
	var body string
	deadline := time.Now().Add(20 * time.Second)
	for {
		w := httptest.NewRecorder()
		client.Gateway().ServeHTTP(w, gwReq("GET", "/network", nil))
		body = w.Body.String()
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		if len(v.Cells) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never appeared on the map: %s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if v.Cells[0].Cell != "u33" || v.Cells[0].Models["m"] != 1 || v.Nodes != 1 || v.Unlocated != 0 {
		t.Fatalf("%s", body)
	}
	for _, leak := range []string{server.host.ID().String(), "52.52", "13.40", "u33d"} {
		if strings.Contains(body, leak) {
			t.Fatalf("/network leaks %q: %s", leak, body)
		}
	}

	// The server counts itself too.
	w := httptest.NewRecorder()
	server.Gateway().ServeHTTP(w, gwReq("GET", "/network", nil))
	json.Unmarshal(w.Body.Bytes(), &v)
	if v.Nodes != 1 || len(v.Cells) != 1 {
		t.Fatalf("server's own view: %s", w.Body)
	}
}
