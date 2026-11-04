package node

import (
	"fmt"
	"regexp"
	"strconv"
)

// A server may cap the images in one prompt (vLLM: --limit-mm-per-prompt) and
// refuse the whole request above the cap. An agent sends every screenshot of a
// session again on each turn, so once a session holds one image too many,
// every later turn would be refused. The cap is the host's own setting, so the
// host applies it: callUpstream learns the limit from the server's first
// refusal, sends that request again with the newest images only, and trims
// later requests for the model before sending them. Clients need no setting
// and never guess another machine's limit.

// vllmImageLimit matches vLLM's refusal, e.g.
// "At most 4 image(s) may be provided in one prompt."
var vllmImageLimit = regexp.MustCompile(`At most (\d+) image\(s\) may be provided in one (?:prompt|request)`)

// imageLimitFrom reads a server's image limit from its refusal message.
func imageLimitFrom(msg string) (int, bool) {
	m := vllmImageLimit.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// imageLimit is the limit learned for one upstream model, if any.
func (n *Node) imageLimit(model string) (int, bool) {
	n.imgMu.Lock()
	defer n.imgMu.Unlock()
	lim, ok := n.imgLimit[model]
	return lim, ok
}

// setImageLimit records a limit our own server stated. Keys are upstream
// models we serve, so the map stays as small as our model list.
func (n *Node) setImageLimit(model string, lim int) {
	n.imgMu.Lock()
	defer n.imgMu.Unlock()
	if n.imgLimit == nil {
		n.imgLimit = map[string]int{}
	}
	n.imgLimit[model] = lim
}

// trimImages keeps only the newest keep images in a chat request, replacing
// each older one with a short text note so the model knows it is missing, and
// returns how many it replaced. It understands OpenAI chat parts (image_url,
// input_image) and Anthropic blocks (image, also inside a tool_result, where
// agents put screenshots).
func trimImages(body map[string]any, keep int) int {
	msgs, _ := body["messages"].([]any)
	var found []slot
	for _, m := range msgs {
		if m, ok := m.(map[string]any); ok {
			found = collectImages(m["content"], found)
		}
	}
	drop := len(found) - max(keep, 0)
	if drop <= 0 {
		return 0
	}
	text := "[image omitted: this server takes no images]"
	if keep > 0 {
		text = fmt.Sprintf("[image omitted: this server takes at most %d images per request, so only the newest %d are kept]", keep, keep)
	}
	for _, s := range found[:drop] {
		s.parts[s.i] = map[string]any{"type": "text", "text": text}
	}
	return drop
}

// slot is one image part: index i in a content array.
type slot struct {
	parts []any
	i     int
}

// collectImages appends the image parts of one message's content, in order.
func collectImages(content any, found []slot) []slot {
	parts, ok := content.([]any)
	if !ok {
		return found // a plain string, or nothing
	}
	for i, p := range parts {
		p, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch p["type"] {
		case "image_url", "input_image", "image":
			found = append(found, slot{parts, i})
		case "tool_result":
			found = collectImages(p["content"], found)
		}
	}
	return found
}
