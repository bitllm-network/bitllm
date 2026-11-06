package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitllm-org/bitllm/internal/models"
	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/host"
)

// recordingUpstream answers every request and keeps the last body it got.
func recordingUpstream(t *testing.T) (up *httptest.Server, last func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var seen map[string]any
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		seen = b
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	return up, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// askPeer sends one request over a real stream and returns the final header.
func askPeer(t *testing.T, h host.Host, n *Node, path, body string) inferHdr {
	t.Helper()
	s := openInfer(t, h, n, protocol.InferProto2, inferReq{Path: path, Body: json.RawMessage(body)}, true)
	br := bufio.NewReader(s)
	for {
		_, hdr := readHdr(t, br, s, 20*time.Second)
		if hdr.Status != 202 {
			return hdr
		}
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestPeerFieldAllowList: a peer's request reaches our server with the listed
// fields only, on each path, and the log counts the rest with capped names.
func TestPeerFieldAllowList(t *testing.T) {
	up, last := recordingUpstream(t)
	n := servedBy(t, up, Config{PeerFields: []string{"repetition_penalty"}})
	h := rawPeer(t, n, nil, "")
	junk := `"prompt_embeds":"x","guided_regex":"x","extra_body":{},"n":8,"repetition_penalty":1.1`
	for path, c := range map[string]struct{ body, kept, gone string }{
		"/chat/completions": {`"messages":[],"add_generation_prompt":false,"continue_final_message":true,"prompt_logprobs":8,"return_token_ids":true,"logit_bias":{}`,
			"add_generation_prompt continue_final_message prompt_logprobs return_token_ids logit_bias messages", "system"},
		"/completions": {`"prompt":"hi","suffix":"x","echo":true,"system":"x"`, "prompt suffix echo", "system"},
		"/messages":    {`"messages":[],"system":"s","top_k":3,"thinking":{},"prompt_logprobs":8`, "messages system top_k thinking", "prompt_logprobs"},
	} {
		logs := captureLog(t)
		hdr := askPeer(t, h, n, path, `{"model":"ds",`+c.body+`,`+junk+`}`)
		if hdr.Status != 200 {
			t.Fatalf("%s: %+v", path, hdr)
		}
		got := last()
		for _, k := range append(strings.Fields(c.kept), "model", "repetition_penalty") {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: %s did not reach the server: %v", path, k, keysOf(got))
			}
		}
		for _, k := range append(strings.Fields(c.gone), "prompt_embeds", "guided_regex", "extra_body", "n") {
			if _, ok := got[k]; ok {
				t.Errorf("%s: %s reached the server: %v", path, k, keysOf(got))
			}
		}
		if !strings.Contains(logs.String(), "dropped ") || !strings.Contains(logs.String(), `"prompt_embeds"`) {
			t.Errorf("%s: nothing logged: %s", path, logs)
		}
	}

	// A long list of names stays short in the log, and a name cannot reach the
	// terminal raw.
	logs := captureLog(t)
	var many []string
	for i := range 200 {
		many = append(many, fmt.Sprintf(`"field\u001b[31m%d%s":1`, i, strings.Repeat("x", 500)))
	}
	if hdr := askPeer(t, h, n, "/chat/completions", `{"model":"ds","messages":[],`+strings.Join(many, ",")+`}`); hdr.Status != 200 {
		t.Fatalf("many fields: %+v", hdr)
	}
	line := logs.String()
	if !strings.Contains(line, "dropped 200 field(s)") || len(line) > 1024 || strings.Contains(line, "\x1b") {
		t.Fatalf("log line: %s", line)
	}
}

// TestLocalKeepsFields: a request from this machine for our own model is not a
// stranger's: every field and every kind of attachment goes through.
func TestLocalKeepsFields(t *testing.T) {
	up, last := recordingUpstream(t)
	n := servedBy(t, up, Config{})
	w := post(n, `{"model":"ds","repetition_penalty":1.1,"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"http://lan/a.png"}},{"type":"video_url","video_url":{"url":"http://lan/a.mp4"}}]}]}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	b, _ := json.Marshal(last())
	for _, want := range []string{"repetition_penalty", "http://lan/a.png", "http://lan/a.mp4"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s did not reach the server: %s", want, b)
		}
	}
}

// TestTemplateKwargs: only scalar values pass, which is all models.json needs.
func TestTemplateKwargs(t *testing.T) {
	up, last := recordingUpstream(t)
	n := servedBy(t, up, Config{})
	h := rawPeer(t, n, nil, "")
	hdr := askPeer(t, h, n, "/chat/completions", `{"model":"ds","messages":[],"chat_template_kwargs":
		{"thinking":false,"budget":3,"mode":"fast","tools":[1],"nested":{"a":1},"none":null,"long":"`+strings.Repeat("x", 500)+`"}}`)
	if hdr.Status != 200 {
		t.Fatalf("%+v", hdr)
	}
	kw, _ := last()["chat_template_kwargs"].(map[string]any)
	if got := strings.Join(keysOf(kw), " "); got != "budget mode thinking" {
		t.Fatalf("kept %q", got)
	}
	hdr = askPeer(t, h, n, "/chat/completions", `{"model":"ds","messages":[],"chat_template_kwargs":"x"}`)
	if _, ok := last()["chat_template_kwargs"]; ok || hdr.Status != 200 {
		t.Fatalf("a non-object passed: %+v %v", hdr, last())
	}
}

// TestManifestFieldsPass: every field the audit's requests carry for a known
// model passes the allow-list, or canaries would quietly change meaning.
func TestManifestFieldsPass(t *testing.T) {
	for model, info := range models.Manifest {
		for k := range info.Extra {
			if !peerFields["/chat/completions"][k] {
				t.Errorf("%s: canary field %s is not allowed", model, k)
			}
		}
	}
	var score map[string]any
	if err := json.Unmarshal(canaryBody("m", "p", "a", false, sampler{}), &score); err != nil {
		t.Fatal(err)
	}
	for k := range score {
		if !peerFields["/chat/completions"][k] {
			t.Errorf("scoring field %s is not allowed", k)
		}
	}
}

// TestPeerMedia: only inline images reach our server, however they are spelled
// and wherever they sit.
func TestPeerMedia(t *testing.T) {
	up, last := recordingUpstream(t)
	n := servedBy(t, up, Config{})
	h := rawPeer(t, n, nil, "")
	const inline = "data:image/png;base64,AAAA"
	for name, c := range map[string]struct {
		path, part string
		keep       bool
	}{
		"data url":        {"/chat/completions", `{"type":"image_url","image_url":{"url":"` + inline + `","detail":"low"}}`, true},
		"data url string": {"/chat/completions", `{"type":"input_image","image_url":"` + inline + `"}`, true},
		"anthropic":       {"/messages", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`, true},
		"in tool result":  {"/messages", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}`, true},
		"http image":      {"/chat/completions", `{"type":"image_url","image_url":{"url":"http://remote.test/a"}}`, false},
		"http input":      {"/chat/completions", `{"type":"input_image","image_url":"http://remote.test/a"}`, false},
		"file url":        {"/chat/completions", `{"type":"image_url","image_url":{"url":"file:///etc/remote.test"}}`, false},
		"anthropic url":   {"/messages", `{"type":"image","source":{"type":"url","url":"http://remote.test/a"}}`, false},
		"video":           {"/chat/completions", `{"type":"video_url","video_url":{"url":"data:video/mp4;base64,remote.test"}}`, false},
		"audio":           {"/chat/completions", `{"type":"audio_url","audio_url":{"url":"http://remote.test/a"}}`, false},
		"input audio":     {"/chat/completions", `{"type":"input_audio","input_audio":{"data":"remote.test","format":"wav"}}`, false},
		"file":            {"/chat/completions", `{"type":"file","file":{"file_id":"remote.test"}}`, false},
		"no type":         {"/chat/completions", `{"image_url":{"url":"http://remote.test/a"}}`, false},
		"hidden in text":  {"/chat/completions", `{"type":"text","text":"hi","uuid":"x","image_url":{"url":"http://remote.test/a"}}`, false},
		"unknown type":    {"/chat/completions", `{"type":"image_embeds","image_embeds":"remote.test"}`, false},
		"tool result url": {"/messages", `{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"http://remote.test/a"}}]}`, false},
		"nested array":    {"/chat/completions", `[{"type":"image_url","image_url":{"url":"http://remote.test/a"}}]`, false},
	} {
		logs := captureLog(t)
		body := `{"model":"ds","max_tokens":4,"messages":[{"role":"user","content":[{"type":"text","text":"look"},` + c.part + `]}]}`
		if hdr := askPeer(t, h, n, c.path, body); hdr.Status != 200 {
			t.Fatalf("%s: %+v", name, hdr)
		}
		b, _ := json.Marshal(last())
		got := string(b)
		if strings.Contains(got, "remote.test") {
			t.Errorf("%s reached the server: %s", name, got)
		}
		if !strings.Contains(got, "look") {
			t.Errorf("%s: text lost: %s", name, got)
		}
		noted := strings.Contains(got, "omitted") || name == "hidden in text"
		if noted == c.keep {
			t.Errorf("%s: keep=%v, sent %s", name, c.keep, got)
		}
		if c.keep && !strings.Contains(got, "AAAA") {
			t.Errorf("%s: the image was lost: %s", name, got)
		}
		if !c.keep && name != "hidden in text" && !strings.Contains(logs.String(), "replaced 1 attachment") {
			t.Errorf("%s: not logged: %s", name, logs)
		}
	}
	// A list of system blocks gets the same walk.
	if hdr := askPeer(t, h, n, "/messages", `{"model":"ds","messages":[],"system":[{"type":"image","source":{"type":"url","url":"http://remote.test/a"}}]}`); hdr.Status != 200 {
		t.Fatalf("system: %+v", hdr)
	}
	if b, _ := json.Marshal(last()); strings.Contains(string(b), "remote.test") {
		t.Fatalf("system: %s", b)
	}
}

// TestOnePrompt: /completions from a peer is one prompt per request.
func TestOnePrompt(t *testing.T) {
	up, _ := recordingUpstream(t)
	n := servedBy(t, up, Config{})
	h := rawPeer(t, n, nil, "")
	for prompt, ok := range map[string]bool{
		`"one"`:         true,
		`["one"]`:       true,
		`[1,2,3]`:       true,
		`["one","two"]`: false,
		`[[1,2],[3]]`:   false,
		`[1,"two"]`:     false,
		`[1.5]`:         false,
		`[]`:            false,
		`{"a":1}`:       false,
		`7`:             false,
		`null`:          false,
	} {
		hdr := askPeer(t, h, n, "/completions", `{"model":"ds","prompt":`+prompt+`}`)
		switch {
		case ok && hdr.Status != 200:
			t.Errorf("%s refused: %+v", prompt, hdr)
		case !ok && (hdr.Status != 400 || hdr.Error != msgOnePrompt):
			t.Errorf("%s answered: %+v", prompt, hdr)
		}
	}
	if hdr := askPeer(t, h, n, "/completions", `{"model":"ds"}`); hdr.Status != 400 {
		t.Errorf("no prompt: %+v", hdr)
	}
}

// TestUpstreamErrorsToPeer: a peer hears little about a failing server, the
// operator's log and a local client hear all of it, and an upstream's own 4xx
// still helps the client after cleaning.
func TestUpstreamErrorsToPeer(t *testing.T) {
	long := strings.Repeat("too long ", 400)
	status, message := 200, ""
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-ds"}]}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, message)
	}))
	defer up.Close()
	n := servedBy(t, up, Config{})
	h := rawPeer(t, n, nil, "")
	body := `{"model":"ds","messages":[]}`
	set := func(s int, m string) {
		mu.Lock()
		status, message = s, m
		mu.Unlock()
	}

	logs := captureLog(t)
	set(400, `{"error":"bad org/model at http://10.0.0.5:8000/v1/x see /opt/models/secret and C:\\models\\x or 10.0.0.5:9000"}`)
	hdr := askPeer(t, h, n, "/chat/completions", body)
	if hdr.Status != 400 || !strings.Contains(hdr.Error, "bad org/model") {
		t.Fatalf("4xx: %+v", hdr)
	}
	for _, leak := range []string{"10.0.0.5", "/opt/models", `C:\`, "http:"} {
		if strings.Contains(hdr.Error, leak) {
			t.Fatalf("4xx leaked %s: %s", leak, hdr.Error)
		}
	}
	if !strings.Contains(logs.String(), "/opt/models/secret") {
		t.Fatalf("detail not in the log: %s", logs)
	}

	set(422, long)
	if hdr := askPeer(t, h, n, "/chat/completions", body); hdr.Status != 422 || len(hdr.Error) > maxPeerErrMsg {
		t.Fatalf("long 4xx: %d, %d bytes", hdr.Status, len(hdr.Error))
	}
	for _, s := range []int{500, 503, 401, 403} {
		set(s, "traceback at /opt/models/secret key sk-abc")
		if hdr := askPeer(t, h, n, "/chat/completions", body); hdr.Status != 502 || hdr.Error != msgUpstream {
			t.Fatalf("%d: %+v", s, hdr)
		}
	}

	// A request written on this machine keeps the full detail.
	set(500, "traceback at /opt/models/secret")
	if w := post(n, body); w.Code != 500 || !strings.Contains(w.Body.String(), "/opt/models/secret") {
		t.Fatalf("local: %d %s", w.Code, w.Body)
	}

	// Our server is gone: no address, no Go error text.
	address := strings.TrimPrefix(up.URL, "http://")
	up.Close()
	logs = captureLog(t)
	hdr = askPeer(t, h, n, "/chat/completions", body)
	if hdr.Status != 502 || hdr.Error != msgUpstream {
		t.Fatalf("transport: %+v", hdr)
	}
	if !strings.Contains(logs.String(), address) {
		t.Fatalf("detail not in the log: %s", logs)
	}
}
