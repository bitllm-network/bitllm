package node

import (
	"strings"
	"unicode"
)

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
