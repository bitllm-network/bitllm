package node

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The log ring is what GET /logs and `bitllm top` read; it must keep the
// newest lines and never grow past its size.
func TestLogRingKeepsTheTail(t *testing.T) {
	r := &logRing{max: 3}
	r.Write([]byte("one\ntwo\n"))
	r.Write([]byte("three\nfour\n"))
	got := strings.Join(r.Tail(2), ",")
	if got != "three,four" {
		t.Fatalf("tail = %q, want the newest two lines", got)
	}
	if n := len(r.Tail(0)); n != 3 {
		t.Fatalf("ring holds %d lines, want its maximum of 3", n)
	}
	r.Write([]byte("five"))
	if got := strings.Join(r.Tail(5), ","); got != "three,four,five" {
		t.Fatalf("after more writes: %q", got)
	}
}

// GET /logs prints what was logged, without the escape sequences a line may hold.
func TestLogsStripsControlCharacters(t *testing.T) {
	const line = "peer said \x1b[2J\x1b[1;1H\x1b]0;title\x07\rdone"
	recentLogs.Write([]byte(line + "\n"))
	w := httptest.NewRecorder()
	(&Node{}).logs(w, httptest.NewRequest("GET", "/logs?n=1", nil))
	got := w.Body.String()
	if want := "peer said [2J[1;1H]0;titledone\n"; got != want {
		t.Fatalf("GET /logs = %q, want %q", got, want)
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("ESC in GET /logs: %q", got)
	}
}
