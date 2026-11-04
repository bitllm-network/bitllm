package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bitllm-network/bitllm/internal/protocol"
)

// Node map. A serving node may say roughly where it is with --location. The
// node turns the coordinates into a geohash cell of locCells characters
// (about 156 km x 156 km) before anything leaves the machine, so the exact
// point is never gossiped. Nodes that set nothing are only counted. Nothing
// is guessed from IP addresses: that would place people who did not ask to
// be placed, and behind relays and tunnels it is often wrong anyway.
//
// GET /network aggregates what this node hears: per model and per cell, node
// counts and free slots. It names no peer ids, so the public map cannot tie a
// node's identity to its region. bitllm.org serves a copy of the bootstrap
// node's answer (scripts/network-json.sh).

const (
	locCells   = 3  // geohash length we announce: ~156 km cells
	maxLocJSON = 16 // longest "loc" we accept from a peer (later versions may send other forms)
)

const geohashAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// parseLocation reads --location: "LAT,LON" in degrees, or a geohash.
// It returns the coarse cell we announce, or "" for "" / "none".
func parseLocation(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "none" {
		return "", nil
	}
	if la, lo, ok := strings.Cut(s, ","); ok {
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(la), 64)
		lon, err2 := strconv.ParseFloat(strings.TrimSpace(lo), 64)
		if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return "", fmt.Errorf("want LAT,LON in degrees, e.g. 52.52,13.40")
		}
		return geohash(lat, lon, locCells), nil
	}
	if len(s) < locCells || strings.Trim(s, geohashAlphabet) != "" {
		return "", fmt.Errorf("want LAT,LON in degrees (e.g. 52.52,13.40) or a geohash of at least %d characters", locCells)
	}
	return s[:locCells], nil
}

// geohash encodes a point as a geohash of the given length.
func geohash(lat, lon float64, length int) string {
	latR, lonR := [2]float64{-90, 90}, [2]float64{-180, 180}
	out := make([]byte, 0, length)
	even, bit, ch := true, 0, 0
	for len(out) < length {
		r, v := &latR, lat
		if even {
			r, v = &lonR, lon
		}
		mid := (r[0] + r[1]) / 2
		ch <<= 1
		if v >= mid {
			ch |= 1
			r[0] = mid
		} else {
			r[1] = mid
		}
		even = !even
		if bit++; bit == 5 {
			out = append(out, geohashAlphabet[ch])
			bit, ch = 0, 0
		}
	}
	return string(out)
}

// cellCenter decodes a geohash to the middle of its cell.
func cellCenter(h string) (lat, lon float64, ok bool) {
	latR, lonR := [2]float64{-90, 90}, [2]float64{-180, 180}
	even := true
	for _, c := range h {
		i := strings.IndexRune(geohashAlphabet, c)
		if i < 0 {
			return 0, 0, false
		}
		for b := 4; b >= 0; b-- {
			r := &latR
			if even {
				r = &lonR
			}
			mid := (r[0] + r[1]) / 2
			if i>>b&1 == 1 {
				r[0] = mid
			} else {
				r[1] = mid
			}
			even = !even
		}
	}
	return (latR[0] + latR[1]) / 2, (lonR[0] + lonR[1]) / 2, true
}

// peerCell is the cell we show for a peer's announced loc, or "" if it gave
// none or one we cannot read. Longer hashes are cut down to our cell size.
func peerCell(loc string) string {
	if len(loc) < locCells || strings.Trim(loc, geohashAlphabet) != "" {
		return ""
	}
	return loc[:locCells]
}

type networkModel struct {
	ID    string `json:"id"`
	Nodes int    `json:"nodes"`
	Free  int    `json:"free"` // slots free right now, summed over nodes
	Max   int    `json:"max"`
}

type networkCell struct {
	Cell   string         `json:"cell"` // geohash, locCells characters
	Lat    float64        `json:"lat"`  // middle of the cell, not of any node
	Lon    float64        `json:"lon"`
	Nodes  int            `json:"nodes"`
	Models map[string]int `json:"models"` // model -> nodes in this cell serving it
}

type networkView struct {
	Updated   time.Time      `json:"updated"`
	Nodes     int            `json:"nodes"`     // serving nodes heard from, including this one
	Unlocated int            `json:"unlocated"` // of those, how many set no --location
	CellKM    int            `json:"cell_km"`   // rough cell size
	Models    []networkModel `json:"models"`
	Cells     []networkCell  `json:"cells"`
	Versions  map[string]int `json:"versions"` // release -> serving nodes
}

// networkSnapshot aggregates the live announces (and our own, if we serve).
func (n *Node) networkSnapshot() networkView {
	anns := []protocol.Announce{}
	for _, a := range n.alive() {
		anns = append(anns, a)
	}
	if own := n.ownAnnounce(); own != nil {
		anns = append(anns, *own)
	}
	return aggregate(anns, time.Now().UTC())
}

func aggregate(anns []protocol.Announce, now time.Time) networkView {
	v := networkView{Updated: now, CellKM: 156, Models: []networkModel{}, Cells: []networkCell{}, Versions: map[string]int{}}
	models := map[string]*networkModel{}
	cells := map[string]*networkCell{}
	for _, a := range anns {
		if len(a.Models) == 0 {
			continue
		}
		v.Nodes++
		ver := a.Version
		if ver == "" {
			ver = "unknown"
		}
		v.Versions[ver]++
		for _, m := range a.Models {
			nm := models[m]
			if nm == nil {
				nm = &networkModel{ID: m}
				models[m] = nm
			}
			nm.Nodes++
			nm.Free += a.Free
			nm.Max += a.Max
		}
		c := peerCell(a.Loc)
		if c == "" {
			v.Unlocated++
			continue
		}
		nc := cells[c]
		if nc == nil {
			lat, lon, _ := cellCenter(c)
			nc = &networkCell{Cell: c, Lat: round2(lat), Lon: round2(lon), Models: map[string]int{}}
			cells[c] = nc
		}
		nc.Nodes++
		for _, m := range a.Models {
			nc.Models[m]++
		}
	}
	for _, m := range models {
		v.Models = append(v.Models, *m)
	}
	sort.Slice(v.Models, func(i, j int) bool {
		if v.Models[i].Nodes != v.Models[j].Nodes {
			return v.Models[i].Nodes > v.Models[j].Nodes
		}
		return v.Models[i].ID < v.Models[j].ID
	})
	for _, c := range cells {
		v.Cells = append(v.Cells, *c)
	}
	sort.Slice(v.Cells, func(i, j int) bool { return v.Cells[i].Cell < v.Cells[j].Cell })
	return v
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// ownAnnounce is what we last gossiped, or nil if we serve nothing right now.
func (n *Node) ownAnnounce() *protocol.Announce {
	a := n.lastAnn.Load()
	if a == nil || len(a.Models) == 0 {
		return nil
	}
	return a
}

func (n *Node) network(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n.networkSnapshot())
}
