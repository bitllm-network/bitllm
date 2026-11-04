package netmap

import (
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
		if got := Geohash(c.lat, c.lon, len(c.want)); got != c.want {
			t.Errorf("Geohash(%v,%v) = %s, want %s", c.lat, c.lon, got, c.want)
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
		got, err := ParseLocation(in)
		if err != nil || got != want {
			t.Errorf("ParseLocation(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"berlin", "91,0", "0,181", "u3", "u3a", "1,2,3"} {
		if _, err := ParseLocation(in); err == nil {
			t.Errorf("ParseLocation(%q): want an error", in)
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
		if got := PeerCell(in); got != want {
			t.Errorf("PeerCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregate(t *testing.T) {
	v := Aggregate([]protocol.Announce{
		{Version: "v0.2.1", Models: []string{"a", "b"}, Free: 1, Max: 2, Loc: "u33"},
		{Version: "v0.2.1", Models: []string{"a"}, Free: 0, Max: 1, Loc: "u33"},
		{Version: "v0.1.13", Models: []string{"a"}, Free: 4, Max: 4},
		{Models: []string{"b"}, Free: 1, Max: 1, Loc: "garbage!"},
		{Models: nil, Loc: "u33"}, // serves nothing right now: not on the map
	}, time.Unix(0, 0))
	if v.Nodes != 4 || v.Unlocated != 2 {
		t.Fatalf("nodes %d unlocated %d", v.Nodes, v.Unlocated)
	}
	if len(v.Models) != 2 || v.Models[0] != (Model{ID: "a", Nodes: 3, Free: 5, Max: 7}) ||
		v.Models[1] != (Model{ID: "b", Nodes: 2, Free: 2, Max: 3}) {
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
