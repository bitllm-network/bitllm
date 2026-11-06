package node

// What a request from a peer may carry to our own server, and what a peer hears
// back when our server fails. Everything here applies to requests that came in
// over the network; requests written on this machine keep every field and every
// detail.

import (
	"log"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	msgOnePrompt   = "one prompt per request"
	msgUpstream    = "upstream unavailable"
	maxPeerErrMsg  = 1 << 10 // an upstream refusal as a peer sees it
	maxLoggedNames = 8       // dropped field names in one log line
	maxLoggedName  = 32
	maxKwargs      = 16 // chat_template_kwargs entries
	maxKwargText   = 64 // longest key or string value in them
)

func fieldSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, f := range names {
		out[f] = true
	}
	return out
}

// peerFields are the fields a peer may send, per path. Operators add more with
// --peer-field. On chat this includes what an audit's scoring request needs
// (canaryBody) and chat_template_kwargs, which models.json sets for some models.
var peerFields = map[string]map[string]bool{
	"/chat/completions": fieldSet("model", "messages", "stream", "stream_options", "temperature", "top_p",
		"max_tokens", "max_completion_tokens", "stop", "seed", "presence_penalty", "frequency_penalty",
		"logit_bias", "logprobs", "top_logprobs", "tools", "tool_choice", "parallel_tool_calls",
		"response_format", "reasoning_effort", "user",
		"add_generation_prompt", "continue_final_message", "prompt_logprobs", "return_token_ids",
		"chat_template_kwargs"),
	"/completions": fieldSet("model", "prompt", "suffix", "stream", "stream_options", "temperature", "top_p",
		"max_tokens", "stop", "seed", "presence_penalty", "frequency_penalty", "logprobs", "echo", "user"),
	"/messages": fieldSet("model", "messages", "system", "max_tokens", "stream", "temperature", "top_p", "top_k",
		"stop_sequences", "metadata", "tools", "tool_choice", "thinking"),
}

// limitPeerRequest cuts a decoded request from a peer down to what peerFields
// allows and to inline images only. An answer other than nil is the refusal.
func (n *Node) limitPeerRequest(path string, body map[string]any) *peerError {
	var dropped []string
	for k := range body {
		if !peerFields[path][k] && !slices.Contains(n.cfg.PeerFields, k) {
			dropped = append(dropped, k)
			delete(body, k)
		}
	}
	dropped = append(dropped, limitTemplateKwargs(body)...)
	replaced := 0
	if path == "/completions" {
		if !onePrompt(body["prompt"]) {
			return &peerError{400, msgOnePrompt}
		}
	} else {
		msgs, _ := body["messages"].([]any)
		for _, m := range msgs {
			if m, ok := m.(map[string]any); ok {
				c, r := limitContent(m["content"])
				if r > 0 {
					m["content"] = c
					replaced += r
				}
			}
		}
		if c, r := limitContent(body["system"]); r > 0 {
			body["system"] = c
			replaced += r
		}
	}
	if len(dropped) > 0 || replaced > 0 {
		log.Printf("peer request to %s: dropped %d field(s) %s, replaced %d attachment(s)",
			path, len(dropped), fieldNames(dropped), replaced)
	}
	return nil
}

// fieldNames is a capped, quoted list for the log: the names come from a peer.
func fieldNames(names []string) string {
	slices.Sort(names)
	var out []string
	for _, s := range names[:min(len(names), maxLoggedNames)] {
		if len(s) > maxLoggedName {
			s = s[:maxLoggedName]
		}
		out = append(out, strconv.Quote(s))
	}
	if len(names) > maxLoggedNames {
		out = append(out, "...")
	}
	return "[" + strings.Join(out, " ") + "]"
}

// limitTemplateKwargs keeps only the scalar entries of chat_template_kwargs and
// returns the names of what it removed.
func limitTemplateKwargs(body map[string]any) []string {
	v, ok := body["chat_template_kwargs"]
	if !ok {
		return nil
	}
	const field = "chat_template_kwargs"
	kw, ok := v.(map[string]any)
	if !ok {
		delete(body, field)
		return []string{field}
	}
	var dropped []string
	kept := 0
	for k, e := range kw {
		if kept < maxKwargs && len(k) <= maxKwargText && scalarKwarg(e) {
			kept++
			continue
		}
		dropped = append(dropped, field+"."+k)
		delete(kw, k)
	}
	return dropped
}

func scalarKwarg(v any) bool {
	switch x := v.(type) {
	case bool, float64:
		return true
	case string:
		return len(x) <= maxKwargText
	}
	return false
}

// onePrompt reports whether a /completions prompt is one prompt: a string, an
// array of one string, or one tokenized prompt.
func onePrompt(v any) bool {
	switch p := v.(type) {
	case string:
		return true
	case []any:
		if len(p) == 0 {
			return false
		}
		if _, ok := p[0].(string); ok {
			return len(p) == 1
		}
		for _, e := range p {
			if f, ok := e.(float64); !ok || f < 0 || f != math.Trunc(f) {
				return false
			}
		}
		return true
	}
	return false
}

// partKeys are the fields kept on a content part that carries no media, by type.
// A server may read media from a field a part's type does not name, so a part
// keeps only what is listed here, whatever else it holds.
var partKeys = map[string][]string{
	"text":              {"text", "cache_control"},
	"input_text":        {"text"},
	"output_text":       {"text"},
	"refusal":           {"refusal"},
	"thinking":          {"thinking", "signature"},
	"redacted_thinking": {"data"},
	"tool_use":          {"id", "name", "input", "cache_control"},
}

// limitContent applies limitPart to one message's content and returns how many
// parts it replaced. A plain string has no parts, so it comes back as it is.
func limitContent(content any) (any, int) {
	switch c := content.(type) {
	case []any:
		total := 0
		for i, p := range c {
			np, r := limitPart(p)
			c[i] = np
			total += r
		}
		return c, total
	case map[string]any:
		np, r := limitPart(c)
		return []any{np}, max(r, 1) // a bare part is not the shape we send on
	}
	return content, 0
}

func omittedPart(text string) (any, int) {
	return map[string]any{"type": "text", "text": text}, 1
}

const (
	noteImage      = "[image omitted: this server takes only inline images]"
	noteAttachment = "[attachment omitted: this server takes only text and inline images]"
)

// limitPart keeps a part only if it holds text, tool calls or an inline image,
// walking into tool results, where agents put screenshots. Anything else becomes
// a short note, so the model knows something is missing.
func limitPart(p any) (any, int) {
	x, ok := p.(map[string]any)
	if !ok {
		if s, ok := p.(string); ok {
			return s, 0
		}
		return omittedPart(noteAttachment)
	}
	if _, typed := x["type"]; !typed {
		if s, ok := x["text"].(string); ok {
			return map[string]any{"type": "text", "text": s}, 0
		}
		return omittedPart(noteAttachment)
	}
	typ, _ := x["type"].(string)
	switch typ {
	case "image_url", "input_image":
		u, ok := inlineImage(x["image_url"])
		if !ok {
			return omittedPart(noteImage)
		}
		out := keepFields(x, "type", "detail")
		out["image_url"] = u
		return out, 0
	case "image":
		src, _ := x["source"].(map[string]any)
		if _, ok := src["data"].(string); !ok || src["type"] != "base64" {
			return omittedPart(noteImage)
		}
		out := keepFields(x, "type", "cache_control")
		out["source"] = keepFields(src, "type", "media_type", "data")
		return out, 0
	case "tool_result":
		out := keepFields(x, "type", "tool_use_id", "is_error", "cache_control")
		if c, ok := x["content"]; ok {
			nc, r := limitContent(c)
			out["content"] = nc
			return out, r
		}
		return out, 0
	}
	if keys, ok := partKeys[typ]; ok {
		out := keepFields(x, keys...)
		out["type"] = typ
		return out, 0
	}
	return omittedPart(noteAttachment)
}

// inlineImage checks an image_url value, a string or {url, detail}, and returns
// it with only those fields if it is a data: URL.
func inlineImage(v any) (any, bool) {
	switch u := v.(type) {
	case string:
		return u, isDataURL(u)
	case map[string]any:
		s, _ := u["url"].(string)
		return keepFields(u, "url", "detail"), isDataURL(s)
	}
	return nil, false
}

func isDataURL(s string) bool {
	return len(s) >= 5 && strings.EqualFold(s[:5], "data:")
}

func keepFields(from map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := from[k]; ok {
			out[k] = v
		}
	}
	return out
}

var (
	urlInMsg  = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>\\]+`)
	pathInMsg = regexp.MustCompile(`(^|[\s"'=(\[,:])(?:~?/|[A-Za-z]:[\\/]|\\\\)[^\s"'<>)\],]*`)
	hostInMsg = regexp.MustCompile(`(?i)\b(?:\d{1,3}(?:\.\d{1,3}){3}|localhost)(?::\d+)?\b`)
)

// cleanUpstreamMsg is an upstream's refusal as a peer may read it: capped, with
// anything that looks like a URL, an address or an absolute path taken out.
func cleanUpstreamMsg(msg string) string {
	const removed = "[removed]"
	msg = urlInMsg.ReplaceAllString(msg, removed)
	msg = pathInMsg.ReplaceAllString(msg, "${1}"+removed)
	msg = hostInMsg.ReplaceAllString(msg, removed)
	if len(msg) > maxPeerErrMsg {
		msg = strings.ToValidUTF8(msg[:maxPeerErrMsg], "")
	}
	return msg
}

// peerFacing is what a peer hears about a failed request to our server. The
// detail goes to the local log only: it can name internal addresses, and an
// upstream's auth refusal can quote part of its key.
func peerFacing(path string, from peer.ID, pe *peerError) *peerError {
	detail := pe.msg
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	log.Printf("%s for peer %s failed: %d %q", path, from, pe.status, detail)
	switch {
	case pe.status < 400, pe.status >= 500, pe.status == 401, pe.status == 403, pe.status == 407:
		return &peerError{502, msgUpstream}
	}
	return &peerError{pe.status, cleanUpstreamMsg(pe.msg)}
}
