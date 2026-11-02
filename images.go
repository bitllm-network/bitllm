package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// trimImages keeps only the newest keep images in a chat request and replaces
// each older one with a short text note, returning the new body and how many
// images it dropped. Servers cap images per prompt (vLLM's
// --limit-mm-per-prompt) and reject the whole request above the cap, and an
// agent re-sends every screenshot of a session on every turn, so without this
// a session stops working for good once it holds one image too many.
//
// It understands OpenAI chat parts (image_url, input_image) and Anthropic
// blocks (image, also inside a tool_result, where agents put screenshots).
// A body it does not change is returned as is, byte for byte.
func trimImages(body []byte, keep int) ([]byte, int) {
	if keep <= 0 || !bytes.Contains(body, []byte("image")) {
		return body, 0
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // re-encode numbers exactly as the client sent them
	var req map[string]any
	if dec.Decode(&req) != nil {
		return body, 0
	}
	msgs, _ := req["messages"].([]any)
	var found []slot
	for _, m := range msgs {
		if m, ok := m.(map[string]any); ok {
			found = collectImages(m["content"], found)
		}
	}
	drop := len(found) - keep
	if drop <= 0 {
		return body, 0
	}
	note := map[string]any{"type": "text", "text": fmt.Sprintf(
		"[image omitted: the server takes at most %d images per request, so only the newest %d are kept]", keep, keep)}
	for _, s := range found[:drop] {
		s.parts[s.i] = note
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(req) != nil {
		return body, 0
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), drop
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
