// Package netmap is the coarse node map: a node may announce a geohash cell
// (about 156 km) instead of an exact position, and every node aggregates the
// announces it hears into per-model and per-cell counts. No peer id appears
// in the aggregate, so the public map cannot tie a node to its region.
package netmap

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bitllm-network/bitllm/internal/protocol"
)

// LocCells is the geohash length we announce: about 156 km cells.
const LocCells = 3

const geohashAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// ParseLocation reads --location: "LAT,LON" in degrees, or a geohash.
// It returns the coarse cell we announce, or "" for "" / "none".
func ParseLocation(s string) (string, error) {
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
		return Geohash(lat, lon, LocCells), nil
	}
	if len(s) < LocCells || strings.Trim(s, geohashAlphabet) != "" {
		return "", fmt.Errorf("want LAT,LON in degrees (e.g. 52.52,13.40) or a geohash of at least %d characters", LocCells)
	}
	return s[:LocCells], nil
}

// Geohash encodes a point as a geohash of the given length.
func Geohash(lat, lon float64, length int) string {
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

// PeerCell is the cell we show for a peer's announced loc, or "" if it gave
// none or one we cannot read. Longer hashes are cut down to our cell size.
func PeerCell(loc string) string {
	if len(loc) < LocCells || strings.Trim(loc, geohashAlphabet) != "" {
		return ""
	}
	return loc[:LocCells]
}

// Model is one model's node and slot counts across the network.
type Model struct {
	ID    string `json:"id"`
	Nodes int    `json:"nodes"`
	Free  int    `json:"free"` // slots free right now, summed over nodes
	Max   int    `json:"max"`
}

// Cell is one geohash cell on the map; it carries no peer ids.
type Cell struct {
	Cell   string         `json:"cell"` // geohash, LocCells characters
	Lat    float64        `json:"lat"`  // middle of the cell, not of any node
	Lon    float64        `json:"lon"`
	Nodes  int            `json:"nodes"`
	Models map[string]int `json:"models"` // model -> nodes in this cell serving it
}

// View is the aggregate /network serves.
type View struct {
	Updated   time.Time      `json:"updated"`
	Nodes     int            `json:"nodes"`     // serving nodes heard from, including this one
	Unlocated int            `json:"unlocated"` // of those, how many set no --location
	Free      int            `json:"free"`      // slots free right now, each node counted once
	Max       int            `json:"max"`       // total slots, each node counted once
	CellKM    int            `json:"cell_km"`   // rough cell size
	Models    []Model        `json:"models"`
	Cells     []Cell         `json:"cells"`
	Versions  map[string]int `json:"versions"` // release -> serving nodes
}

func Aggregate(anns []protocol.Announce, now time.Time) View {
	v := View{Updated: now, CellKM: 156, Models: []Model{}, Cells: []Cell{}, Versions: map[string]int{}}
	models := map[string]*Model{}
	cells := map[string]*Cell{}
	for _, a := range anns {
		if len(a.Models) == 0 {
			continue
		}
		v.Nodes++
		// A node's slots are shared by all its models, so the rows in Models
		// count it once per model. These totals count it once.
		v.Free += a.Free
		v.Max += a.Max
		ver := a.Version
		if ver == "" {
			ver = "unknown"
		}
		v.Versions[ver]++
		for _, m := range a.Models {
			nm := models[m]
			if nm == nil {
				nm = &Model{ID: m}
				models[m] = nm
			}
			nm.Nodes++
			nm.Free += a.Free
			nm.Max += a.Max
		}
		c := PeerCell(a.Loc)
		if c == "" {
			v.Unlocated++
			continue
		}
		nc := cells[c]
		if nc == nil {
			lat, lon, _ := cellCenter(c)
			nc = &Cell{Cell: c, Lat: round2(lat), Lon: round2(lon), Models: map[string]int{}}
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
