package node

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// recentLogs keeps the tail of what this node logged, so a local view can show
// it: `bitllm top` reads it through GET /logs, and anything else watching the
// node locally can read the same. main() sends log output through it.
var recentLogs = &logRing{max: 500}

// LogOutput is where the node keeps its recent log lines for GET /logs and
// `bitllm top`. Pass it to log.SetOutput alongside stderr.
var LogOutput = io.Writer(recentLogs)

type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// Write implements io.Writer, so log.SetOutput can tee into it.
func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.Split(strings.TrimSuffix(string(p), "\n"), "\n")...)
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = append(r.lines[:0], r.lines[over:]...)
	}
	return len(p), nil
}

// Tail returns the last n lines, oldest first.
func (r *logRing) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	return append([]string(nil), r.lines[len(r.lines)-n:]...)
}

// logs serves the node's recent log lines as plain text.
func (n *Node) logs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if limit <= 0 || limit > recentLogs.max {
		limit = 200
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, line := range recentLogs.Tail(limit) {
		fmt.Fprintln(w, printableLine(line))
	}
}

// printableLine drops what a terminal would act on instead of print: control
// characters (ESC among them), bidi overrides and line separators. So printing
// GET /logs in a terminal only ever prints.
func printableLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), r == '\u2028', r == '\u2029':
			return -1
		}
		return r
	}, s)
}
