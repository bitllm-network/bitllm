package node

import (
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
