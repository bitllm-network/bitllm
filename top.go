package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// `bitllm top` is the local view of a running node: peers, the work exchanged
// with each of them, the models being served, and the last log lines, redrawn
// once a second. It only talks to the local gateway over HTTP, so it works
// whatever started the node (a terminal, tmux, a service) and never touches
// the node's own state.

// nodeStatus is the part of GET /status that top shows.
type nodeStatus struct {
	Version    string                 `json:"version"`
	ID         string                 `json:"id"`
	Conns      []nodeConn             `json:"conns"`
	Serving    []string               `json:"serving"`
	UpstreamUp bool                   `json:"upstream_up"`
	Inflight   int64                  `json:"inflight"`
	Uptime     int64                  `json:"uptime"` // seconds
	Ledger     map[string]nodeAccount `json:"ledger"`
}

type nodeConn struct {
	Peer    string `json:"peer"`
	Addr    string `json:"addr"`
	Dir     string `json:"dir"`
	Relayed bool   `json:"relayed"`
}

type nodeAccount struct {
	Gave int64 `json:"gave"` // work this peer did for us
	Took int64 `json:"took"` // work we did for this peer
}

// view is what top redraws: the last status, the last logs, and the work rate.
type view struct {
	base     string
	client   *http.Client
	every    time.Duration
	started  time.Time
	status   *nodeStatus
	err      error
	logs     []string
	work     []int64 // credits exchanged per refresh, oldest first
	lastWork int64
	haveWork bool
}

func topCmd(args []string) {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	gateway := fs.String("gateway", "127.0.0.1:7778", "local gateway to watch")
	every := fs.Duration("interval", time.Second, "refresh interval")
	fs.Parse(args)

	base := *gateway
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	v := &view{
		base:    strings.TrimRight(base, "/"),
		client:  &http.Client{Timeout: 2 * time.Second},
		every:   *every,
		started: time.Now(),
	}

	if !term.IsTerminal(int(os.Stdout.Fd())) {
		// Not a terminal (a pipe, a log): print one frame with plain newlines
		// and no escapes beyond colour, so it reads like any other command.
		v.refresh()
		fmt.Fprintln(os.Stdout, strings.ReplaceAll(v.render(100, 40), "\r\n", "\n"))
		if v.err != nil {
			os.Exit(1)
		}
		return
	}
	runTop(v)
}

// refresh reads the gateway once.
func (v *view) refresh() {
	resp, err := v.client.Get(v.base + "/status")
	if err == nil {
		var st nodeStatus
		if resp.StatusCode == 200 {
			err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st)
		} else {
			err = fmt.Errorf("gateway answered %s", resp.Status)
		}
		resp.Body.Close()
		if err != nil {
			v.status, v.err = nil, err
		} else {
			v.status, v.err = &st, nil
			v.sample(&st)
		}
	} else {
		v.status, v.err = nil, err
	}

	if resp, err := v.client.Get(v.base + "/logs?n=120"); err == nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()
		if resp.StatusCode == 200 {
			v.logs = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		}
	}
}

// sample records how many credits were exchanged since the last refresh, for
// the rate line. Credits live in the ledger, so this works whether the node
// serves, uses, or does both.
func (v *view) sample(st *nodeStatus) {
	var total int64
	for _, a := range st.Ledger {
		total += a.Gave + a.Took
	}
	if v.haveWork {
		v.work = append(v.work, total-v.lastWork)
	} else {
		v.work = append(v.work, 0)
		v.haveWork = true
	}
	v.lastWork = total
	if keep := int(time.Minute / v.every); len(v.work) > max(keep, 2) {
		v.work = v.work[len(v.work)-max(keep, 2):]
	}
}

func runTop(v *view) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// q (or ctrl-c, which raw mode turns into a byte) quits.
	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
		go func() {
			buf := make([]byte, 1)
			for {
				if n, err := os.Stdin.Read(buf); err != nil || n == 0 {
					return
				}
				if buf[0] == 'q' || buf[0] == 'Q' || buf[0] == 3 {
					stop()
					return
				}
			}
		}()
	}

	out := os.Stdout
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")       // alternate screen, no cursor
	defer fmt.Fprint(out, "\x1b[?25h\x1b[?1049l") // cursor back, screen restored

	tick := time.NewTicker(v.every)
	defer tick.Stop()
	for {
		v.refresh()
		w, h := termSize()
		// One column short of the edge: a line that fills the last column
		// leaves the terminal in its pending-wrap state, and the newline then
		// lands on the wrong row (tmux wraps early).
		fmt.Fprint(out, "\x1b[H", v.render(w-1, h), "\x1b[J")
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func termSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 20 || h < 10 {
		return 100, 30
	}
	return w, h
}

// ANSI, kept to the eight colours every terminal has so it reads the same in a
// light and a dark theme.
const (
	reset = "\x1b[0m"
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	green = "\x1b[32m"
	red   = "\x1b[31m"
)

func paint(code, s string) string { return code + s + reset }

// render draws one frame of h-1 lines, each at most w columns wide: the last
// row is left for the caller, so redrawing a full screen never scrolls it.
func (v *view) render(w, h int) string {
	if v.status == nil {
		return v.renderDown(w, h)
	}
	st := v.status
	lines := make([]string, 0, h)

	// A node from before /status carried uptime: show how long we have watched
	// instead of a wrong zero.
	up := clock(time.Since(v.started))
	if st.Uptime > 0 {
		up = clock(time.Duration(st.Uptime) * time.Second)
	}
	head := fmt.Sprintf("bitllm %s   %s   up %s   peers %d   inflight %d",
		st.Version, shortID(st.ID), up, len(st.Conns), st.Inflight)
	lines = append(lines, paint(bold, fit(head, w)), rule(w))

	// Peers. Leave room for the sections below.
	peerRows := min(len(st.Conns), max((h-14)/3, 0))
	lines = append(lines, title("PEERS", fmt.Sprintf("%d connected", len(st.Conns)), w))
	if len(st.Conns) == 0 {
		lines = append(lines, paint(dim, fit("  none yet: the node finds peers as it joins the network", w)))
	}
	for _, c := range sortedConns(st.Conns)[:peerRows] {
		lines = append(lines, fit(peerLine(c), w))
	}
	if len(st.Conns) > peerRows {
		lines = append(lines, paint(dim, fit(fmt.Sprintf("  … %d more", len(st.Conns)-peerRows), w)))
	}
	lines = append(lines, rule(w))

	// What this node shares, if anything.
	serving := "not sharing a model, using the network only"
	status, colour := "idle", dim
	if len(st.Serving) > 0 {
		serving = strings.Join(st.Serving, ", ")
		status, colour = "upstream up", green
		if !st.UpstreamUp {
			status, colour = "upstream down", red
		}
	}
	lines = append(lines, servingLine(serving, status, colour, w), rule(w))

	// The ledger: who did work for us, and what we did for them.
	workRows := min(len(st.Ledger), max((h-14)/4, 0))
	lines = append(lines, title("WORK EXCHANGED", "credits", w))
	lines = append(lines, workLine("", "did for you", "you did", "net", w, true))
	var gave, took int64
	peers := sortedAccounts(st.Ledger)
	for _, p := range peers {
		gave += st.Ledger[p].Gave
		took += st.Ledger[p].Took
	}
	for _, p := range peers[:workRows] {
		a := st.Ledger[p]
		lines = append(lines, workLine(shortID(p), credit(a.Gave), credit(a.Took), netCredit(a.Gave-a.Took), w, false))
	}
	if len(peers) > workRows {
		lines = append(lines, paint(dim, fit(fmt.Sprintf("  … %d more", len(peers)-workRows), w)))
	}
	lines = append(lines, workLine("total", credit(gave), credit(took), netCredit(gave-took), w, true))
	lines = append(lines, rule(w))

	// Rate of work exchanged, as a sparkline over the last minute.
	lines = append(lines, fit("RATE     "+v.bar()+"   "+v.rate(), w), rule(w))

	// Everything else is the log, newest last, aligned to the bottom.
	room := h - len(lines) - 1 // one line for the footer
	if room >= 2 {
		lines = append(lines, title("LOG", "last lines", w))
		logs := v.logs
		if len(logs) > room-1 {
			logs = logs[len(logs)-(room-1):]
		}
		if len(logs) == 0 {
			lines = append(lines, paint(dim, fit("  nothing logged yet", w)))
		}
		for _, l := range logs {
			lines = append(lines, fit("  "+l, w))
		}
	}

	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	if len(lines) > h-2 {
		lines = lines[:h-2]
	}
	foot := "q quit   ·   the node keeps running"
	lines = append(lines, paint(dim, fit(foot, w)))
	// Erase past each line so a frame drawn after the window got narrower
	// leaves no tail of the wider one, and \r\n (not \n) because the
	// terminal is in raw mode. The frame is h-1 lines and ends without a
	// newline, so redrawing never scrolls: the last row is the caller's \x1b[J.
	return strings.Join(lines, "\x1b[K\r\n")
}

// renderDown is the frame for a gateway that is not answering: the node may be
// stopped, or about to come back, so top keeps watching either way.
func (v *view) renderDown(w, h int) string {
	lines := []string{
		paint(bold, fit("bitllm top", w)),
		rule(w),
		"",
		fit("  no answer from "+v.base, w),
		fit("  "+errText(v.err), w),
		"",
		paint(dim, fit("  start the node in another terminal:  bitllm", w)),
		paint(dim, fit("  this view reconnects by itself when it does", w)),
	}
	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	if len(lines) > h-2 {
		lines = lines[:h-2]
	}
	return strings.Join(append(lines, paint(dim, fit("q quit", w))), "\x1b[K\r\n")
}

func title(left, right string, w int) string {
	gap := w - len([]rune(left)) - len([]rune(right))
	if gap < 1 {
		return paint(bold, fit(left, w))
	}
	return paint(bold, left) + strings.Repeat(" ", gap) + paint(dim, right)
}

func rule(w int) string { return paint(dim, strings.Repeat("─", w)) }

// servingLine puts the shared models on the left and the upstream state on
// the right, padded so the state sits against the edge.
func servingLine(serving, status, colour string, w int) string {
	left := fit("SERVING  "+serving, max(w-len([]rune(status))-1, 0))
	gap := max(w-len([]rune(left))-len([]rune(status)), 0)
	return left + strings.Repeat(" ", gap) + paint(colour, status)
}

func peerLine(c nodeConn) string {
	dir := "in"
	if strings.HasPrefix(strings.ToLower(c.Dir), "out") {
		dir = "out"
	}
	via := "direct"
	if c.Relayed {
		via = "relay"
	}
	return fmt.Sprintf("  %-15s  %-3s  %-6s  %s", shortID(c.Peer), dir, via, shortAddr(c.Addr))
}

// shortAddr keeps the host of a multiaddr, which is the part a person reads.
func shortAddr(addr string) string {
	parts := strings.Split(strings.TrimPrefix(addr, "/"), "/")
	host := addr
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "ip4", "ip6", "dns", "dns4", "dns6":
			host = parts[i+1]
		}
	}
	if i := strings.Index(host, "/"); i > 0 {
		host = host[:i]
	}
	return host
}

func workLine(name, gave, took, net string, w int, head bool) string {
	const num = 14
	line := fmt.Sprintf("  %-15s%*s%*s%*s", name, num, gave, num, took, num, net)
	if head {
		return paint(dim, fit(line, w))
	}
	return fit(line, w)
}

func credit(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + group(s[1:])
	}
	return group(s)
}

func netCredit(n int64) string {
	if n > 0 {
		return "+" + group(fmt.Sprint(n))
	}
	return credit(n)
}

// group inserts thousands separators: 8605792 -> 8,605,792.
func group(s string) string {
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
		if len(s) > lead {
			b.WriteString(",")
		}
	}
	for i := lead; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteString(",")
		}
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:9] + "…" + id[len(id)-5:]
}

func clock(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	if u := strings.Index(err.Error(), ": "); u > 0 {
		return err.Error()[u+2:]
	}
	return err.Error()
}

// bar is a sparkline of credits exchanged per refresh over the last minute.
func (v *view) bar() string {
	const levels = "▁▂▃▄▅▆▇█"
	if len(v.work) == 0 {
		return strings.Repeat(string([]rune(levels)[0]), 1)
	}
	var peak int64
	for _, n := range v.work {
		if n > peak {
			peak = n
		}
	}
	var b strings.Builder
	for _, n := range v.work {
		i := 0
		if peak > 0 {
			i = int(n * int64(len([]rune(levels))-1) / peak)
		}
		b.WriteString(string([]rune(levels)[i]))
	}
	return b.String()
}

func (v *view) rate() string {
	var sum int64
	for _, n := range v.work {
		sum += n
	}
	secs := float64(len(v.work)) * v.every.Seconds()
	if secs <= 0 {
		return "0 credits/min"
	}
	return group(fmt.Sprint(int64(float64(sum)*60/secs))) + " credits/min"
}

func sortedConns(conns []nodeConn) []nodeConn {
	out := append([]nodeConn(nil), conns...)
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
}

func sortedAccounts(m map[string]nodeAccount) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := m[out[i]], m[out[j]]
		return abs64(a.Gave-a.Took) > abs64(b.Gave-b.Took)
	})
	return out
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// fit truncates s to w columns, counting runes so a multi-byte id or a box
// character does not shift the rest of the line.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return string(r[:1])
	}
	return string(r[:w-1]) + "…"
}
