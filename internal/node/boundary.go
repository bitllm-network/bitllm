package node

import (
	"bufio"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/libp2p/go-libp2p/core/network"
)

// What a peer's answer may make the client hold. These are vars so tests can
// shorten them.
var (
	maxPeerBody int64 = 64 << 20        // one non-streamed answer
	maxPeerLine       = 8 << 20         // one line of a streamed answer
	peerStall         = 5 * time.Minute // longest a peer may stay silent while sending a body
)

// Ways a peer's answer can end without being a whole answer.
var (
	errPeerTooLarge = errors.New("the answer is over the size limit")
	errPeerNotJSON  = errors.New("the answer is not JSON")
	errPeerNoAnswer = errors.New("the peer accepted the request and sent no answer")
	errPeerCut      = errors.New("the stream ended before its last event")
	errLocalGone    = errors.New("the local client went away")
)

// stallReader restarts the stream's read deadline before each read, once stall
// is set, so an answer may take as long as it keeps moving.
type stallReader struct {
	s     network.Stream
	stall time.Duration
}

func (r *stallReader) Read(b []byte) (int, error) {
	if r.stall > 0 {
		r.s.SetReadDeadline(time.Now().Add(r.stall))
	}
	return r.s.Read(b)
}

// readLine reads one line, newline included, into buf. A line over limit bytes
// is errPeerTooLarge; limit 0 means no limit. Like ReadBytes, it returns what
// it has along with the error that ended the line.
func readLine(r *bufio.Reader, buf []byte, limit int) ([]byte, error) {
	buf = buf[:0]
	for {
		part, err := r.ReadSlice('\n')
		if limit > 0 && len(buf)+len(part) > limit {
			return buf[:0], errPeerTooLarge
		}
		buf = append(buf, part...)
		if err != bufio.ErrBufferFull {
			return buf, err
		}
	}
}

// maxPeerText is the most of a peer's error message we keep. A server's own
// messages fit well inside it, including the access refusals isBusy and
// notAllowed read and a model server's explanation of what it refused.
const maxPeerText = 2048

// cleanPeerText makes text from a peer safe to print: only printable
// characters stay, so control characters (ESC among them) and invalid UTF-8
// are gone, line breaks and tabs become spaces so a message stays on one log
// line, and the result is at most max bytes, cut between characters.
func cleanPeerText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		case r == unicode.ReplacementChar || !unicode.IsGraphic(r):
			continue
		}
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// peerStatus maps the status a peer sent to one we pass on to the local client.
func peerStatus(status int) int {
	switch status {
	case 400, 403, 404, 413, 422, 429, 500, 502, 503, 504:
		return status
	}
	return 502
}
