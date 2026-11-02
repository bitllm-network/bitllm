package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json %v: %s", err, s)
	}
	return m
}

func countImages(body map[string]any) int {
	msgs, _ := body["messages"].([]any)
	n := 0
	for _, m := range msgs {
		if m, ok := m.(map[string]any); ok {
			n += len(collectImages(m["content"], nil))
		}
	}
	return n
}

// sevenImages is an OpenAI chat request with images IMG0 (oldest) to IMG6.
func sevenImages(model string) string {
	var msgs []string
	for i := 0; i < 7; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":"shot %d"},`+
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,IMG%d"}}]}`, i, i))
	}
	return `{"model":"` + model + `","max_tokens":20,"messages":[` + strings.Join(msgs, ",") + `]}`
}

func TestTrimImages(t *testing.T) {
	b := decode(t, sevenImages("m"))
	if d := trimImages(b, 4); d != 3 || countImages(b) != 4 {
		t.Fatalf("dropped %d, %d left", d, countImages(b))
	}
	out, _ := json.Marshal(b)
	s := string(out)
	for i := 0; i < 7; i++ {
		if kept := strings.Contains(s, fmt.Sprintf("IMG%d", i)); kept != (i >= 3) {
			t.Fatalf("image %d kept=%v: %s", i, kept, s)
		}
	}
	if strings.Count(s, "only the newest 4 are kept") != 3 || !strings.Contains(s, "shot 0") {
		t.Fatalf("notes or text lost: %s", s)
	}
	if d := trimImages(b, 4); d != 0 {
		t.Fatalf("second trim dropped %d", d)
	}
	b = decode(t, sevenImages("m"))
	if d := trimImages(b, 0); d != 7 || !strings.Contains(fmt.Sprint(b), "takes no images") {
		t.Fatalf("limit 0: dropped %d", d)
	}

	// Anthropic: agents put screenshots inside tool results.
	b = decode(t, `{"model":"m","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A1"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A2"}}]}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"plain text result"}]},
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A3"}}]}]}`)
	if d := trimImages(b, 1); d != 2 || countImages(b) != 1 {
		t.Fatalf("anthropic: dropped %d", d)
	}
	out, _ = json.Marshal(b)
	if s := string(out); !strings.Contains(s, `"A3"`) || strings.Contains(s, `"A1"`) || strings.Contains(s, `"A2"`) {
		t.Fatalf("anthropic kept the wrong images: %s", s)
	}
}

func TestImageLimitFrom(t *testing.T) {
	for msg, want := range map[string]int{
		`{"error":{"message":"At most 4 image(s) may be provided in one prompt. (parameter=image)","code":400}}`: 4,
		`At most 1 image(s) may be provided in one request.`:                                                     1,
		`At most 0 image(s) may be provided in one prompt.`:                                                      0,
	} {
		if got, ok := imageLimitFrom(msg); !ok || got != want {
			t.Fatalf("%q: %d %v", msg, got, ok)
		}
	}
	for _, msg := range []string{"bad json", `At most 2 video(s) may be provided in one prompt.`, ""} {
		if _, ok := imageLimitFrom(msg); ok {
			t.Fatalf("%q read as an image limit", msg)
		}
	}
}

// TestImageLimit: a host shares a vLLM server started with
// --limit-mm-per-prompt image=4, which refuses any request with more. The
// host learns the limit from the first refusal and answers with the newest
// four images; later requests are trimmed before they reach the server.
// The client sends the request as is.
func TestImageLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls, seen atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-vl"}]}`)
			return
		}
		calls.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		seen.Store(int64(countImages(body)))
		if seen.Load() > 4 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"At most 4 image(s) may be provided in one prompt. (parameter=image)","type":"BadRequestError","param":"image","code":400}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":40,"completion_tokens":10}}`)
	}))
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1", Models: map[string]string{"vl": "local-vl"}})
	client := testNode(t, ctx, Config{Bootstrap: bs})
	for deadline := time.Now().Add(20 * time.Second); len(client.candidates("vl")) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no discovery")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if w := post(client, sevenImages("vl")); w.Code != 200 || seen.Load() != 4 || calls.Load() != 2 {
		t.Fatalf("first request: %d, %d upstream calls, last saw %d images: %s", w.Code, calls.Load(), seen.Load(), w.Body)
	}
	if w := post(client, sevenImages("vl")); w.Code != 200 || seen.Load() != 4 || calls.Load() != 3 {
		t.Fatalf("second request should be trimmed before sending: %d, %d upstream calls: %s", w.Code, calls.Load(), w.Body)
	}
}
