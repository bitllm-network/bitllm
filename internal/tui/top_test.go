package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// frame splits a rendered frame into visible lines, the way a terminal would
// see them: no escape sequences, CRLF between rows, no trailing newline.
func frame(t *testing.T, s string) []string {
	t.Helper()
	if strings.HasSuffix(s, "\n") {
		t.Fatalf("frame ends with a newline: drawing it would scroll the screen")
	}
	lines := strings.Split(s, "\x1b[K\r\n")
	for i, l := range lines {
		lines[i] = ansi.ReplaceAllString(l, "")
	}
	return lines
}

func testView() *view {
	v := &view{
		base:    "http://127.0.0.1:7778",
		every:   time.Second,
		started: time.Now().Add(-3 * time.Minute),
		status: &nodeStatus{
			Version: "v1.2.3", ID: "12D3KooWB3zAyx9hjQ47jKJp5Yyxzcgzw3dAd7ZExdiaY8NPFw2o",
			Serving: []string{"deepseek-v4.1-flash-1017"}, UpstreamUp: true, Inflight: 2, Uptime: 200,
			Conns: []nodeConn{
				{Peer: "12D3KooWSjSmNqem7jVEwc3scunEjpTqSZPgPdhMx6VbZ5piETF", Addr: "/ip4/1.2.3.4/tcp/4001", Dir: "out", Relayed: true},
				{Peer: "12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib", Addr: "/dns4/boot.bitllm.org/tcp/443/wss", Dir: "in"},
			},
			Ledger: map[string]nodeAccount{"12D3KooWSjSmNqem7jVEwc3scunEjpTqSZPgPdhMx6VbZ5piETF": {Gave: 8605792, Took: 12}},
		},
		logs: []string{"2026/11/01 17:29:32 gateway http://127.0.0.1:7778/v1"},
		work: []int64{0, 400, 1200},
	}
	return v
}

// A frame must fill the screen without ever writing past it: one line too many
// scrolls the whole view up by one row, which is what a lost header looks like.
func TestRenderFitsTheScreen(t *testing.T) {
	for _, size := range [][2]int{{100, 40}, {80, 24}, {60, 16}, {200, 60}, {40, 12}} {
		w, h := size[0], size[1]
		lines := frame(t, testView().render(w, h))
		if len(lines) != h-1 {
			t.Fatalf("%dx%d: %d lines, want %d", w, h, len(lines), h-1)
		}
		for i, l := range lines {
			if n := len([]rune(l)); n > w {
				t.Fatalf("%dx%d: line %d is %d columns wide: %q", w, h, i, n, l)
			}
		}
	}
}

func TestRenderDownFitsTheScreen(t *testing.T) {
	v := &view{base: "http://127.0.0.1:7778", err: errOf("dial tcp 127.0.0.1:7778: connect: connection refused")}
	for _, size := range [][2]int{{100, 40}, {60, 16}} {
		w, h := size[0], size[1]
		lines := frame(t, v.render(w, h))
		if len(lines) != h-1 {
			t.Fatalf("%dx%d: %d lines, want %d", w, h, len(lines), h-1)
		}
		if !strings.Contains(strings.Join(lines, "\n"), "no answer from") {
			t.Fatalf("%dx%d: the stopped-node frame does not say what happened", w, h)
		}
	}
}

// The header is the first thing a person looks at, so it must survive even a
// very narrow window.
func TestRenderKeepsTheHeader(t *testing.T) {
	lines := frame(t, testView().render(60, 24))
	if !strings.HasPrefix(lines[0], "bitllm v1.2.3") {
		t.Fatalf("header missing or truncated: %q", lines[0])
	}
}

func TestFitCountsRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 5, "abcd…"},
		{"12D3KooWB3zAyx9hjQ", 9, "12D3KooW…"},
		{"híjklmn", 4, "híj…"},
		{"abc", 0, ""},
		{"a\x1b[2Jb", 20, "a[2Jb"},
		{"\x1b\x1b\x1b\x1babcdef", 5, "abcd…"}, // cut after cleaning, or the width is wrong
		{"a\u009b31mb\u202ec\u2028d\te\r\n", 20, "a31mbcd e"},
		{"bad\x9bbyte", 20, "bad\ufffdbyte"},
	} {
		if got := fit(tc.in, tc.w); got != tc.want {
			t.Errorf("fit(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
}

func TestGroup(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0", "0"}, {"999", "999"}, {"1000", "1,000"}, {"8605792", "8,605,792"}, {"123456", "123,456"},
	} {
		if got := group(tc.in); got != tc.want {
			t.Errorf("group(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got, want := credit(-1234), "-1,234"; got != want {
		t.Errorf("credit(-1234) = %q, want %q", got, want)
	}
	if got, want := netCredit(1234), "+1,234"; got != want {
		t.Errorf("netCredit(1234) = %q, want %q", got, want)
	}
}

func TestShortAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/ip4/104.21.63.172/tcp/4001", "104.21.63.172"},
		{"/dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3", "boot.bitllm.org"},
		{"/ip6/fd12:f513::1/udp/4001/quic-v1", "fd12:f513::1"},
		{"/ip4/127.0.0.1/udp/4001/quic-v1/p2p/12D3", "127.0.0.1"},
	} {
		if got := shortAddr(tc.in); got != tc.want {
			t.Errorf("shortAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClock(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{75 * time.Second, "1m15s"},
		{2*time.Hour + 3*time.Minute, "2h03m"},
	} {
		if got := clock(tc.in); got != tc.want {
			t.Errorf("clock(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Escape sequences in /logs or /status must reach the screen as plain text,
// while the view's own colours and line ends stay. Everything below goes through
// real JSON and a real HTTP round trip, the way the text arrives.
func TestRefreshStripsControlCharacters(t *testing.T) {
	const seq = "\x1b[2J\x1b[1;1H\x1b]0;title\x07\x1b[?1049l\u009b31m\rTEXT\x08\x7f"
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/status":
			json.NewEncoder(w).Encode(map[string]any{
				"version": "v1" + seq, "id": "id" + seq, "serving": []string{"model" + seq},
				"upstream_up": true, "uptime": 5,
				"conns":  []map[string]any{{"peer": "peer" + seq, "addr": "/ip4/1.2.3.4" + seq, "dir": "out" + seq}},
				"ledger": map[string]any{"ledger-peer" + seq: map[string]any{"gave": 1, "took": 2}},
			})
		case "/logs":
			fmt.Fprintf(w, "2026/11/05 first line\n2026/11/05 peer said: %s\n", seq)
		}
	}))
	defer srv.Close()
	v := &view{base: srv.URL, key: "sekret", client: srv.Client(), every: time.Second, started: time.Now()}
	v.refresh()
	if v.status == nil {
		t.Fatalf("no status: %v", v.err)
	}
	if strings.Contains(strings.Join(v.logs, ""), "\x1b") || strings.Contains(v.status.Version, "\x1b") {
		t.Fatalf("control characters entered the view: %q %q", v.logs, v.status.Version)
	}
	if got, want := v.logs[1], "2026/11/05 peer said: [2J[1;1H]0;title[?1049l31mTEXT"; got != want {
		t.Fatalf("log line = %q, want %q", got, want)
	}
	for _, k := range keys {
		if k != "Bearer sekret" {
			t.Fatalf("request without the gateway key: %q", k)
		}
	}
	assertOnlyOwnEscapes(t, v.render(160, 40))

	// Text set some other way, or an error from the gateway, is cleaned by fit.
	v.logs = []string{seq}
	v.status.Version = seq
	assertOnlyOwnEscapes(t, v.render(160, 40))
	v.status, v.err = nil, errOf("x: gateway said "+seq)
	v.base += seq
	assertOnlyOwnEscapes(t, v.render(160, 40))
}

// assertOnlyOwnEscapes fails if a frame holds any control character other than
// the few escape codes the view itself writes.
func assertOnlyOwnEscapes(t *testing.T, s string) {
	t.Helper()
	if !strings.Contains(s, bold) {
		t.Fatalf("the view's own escape codes are gone: %q", s)
	}
	rest := s
	for _, own := range []string{reset, dim, bold, green, red, "\x1b[K", "\r\n"} {
		rest = strings.ReplaceAll(rest, own, "")
	}
	for _, r := range rest {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			t.Fatalf("control character %#x in the frame: %q", r, rest)
		}
	}
}

func TestRefreshSaysWhenTheKeyIsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	v := &view{base: srv.URL, client: srv.Client(), every: time.Second, started: time.Now()}
	v.refresh()
	if v.err == nil || !strings.Contains(v.err.Error(), "--gateway-key") {
		t.Fatalf("err = %v", v.err)
	}
}

type errOf string

func (e errOf) Error() string { return string(e) }
