package node

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/netmap"
)

// A server with --location shows up on a client's /network as a cell, with no
// peer id and no finer position anywhere in the answer.
func TestNetworkMap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := mockUpstream(t, "m")
	defer up.Close()

	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	loc, _ := netmap.ParseLocation("52.5201,13.4049")
	server := testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1", Location: loc})
	client := testNode(t, ctx, Config{Bootstrap: bs})

	var v netmap.View
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
