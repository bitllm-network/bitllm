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

func countImages(t *testing.T, body []byte) int {
	t.Helper()
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("bad json %v: %s", err, body)
	}
	n := 0
	for _, m := range req.Messages {
		n += len(collectImages(m["content"], nil))
	}
	return n
}

func TestTrimImages(t *testing.T) {
	img := func(i int) string {
		return fmt.Sprintf(`{"type":"image_url","image_url":{"url":"data:image/png;base64,IMG%d"}}`, i)
	}
	var msgs []string
	for i := 1; i <= 6; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":"shot %d <b>"},%s]}`, i, img(i)))
	}
	body := []byte(`{"model":"m","seed":12345678901234567890,"temperature":0.1,"messages":[` + strings.Join(msgs, ",") + `]}`)

	out, dropped := trimImages(body, 4)
	if dropped != 2 || countImages(t, out) != 4 {
		t.Fatalf("dropped %d, %d left: %s", dropped, countImages(t, out), out)
	}
	s := string(out)
	if strings.Contains(s, "IMG1") || strings.Contains(s, "IMG2") || !strings.Contains(s, "IMG3") || !strings.Contains(s, "IMG6") {
		t.Fatalf("kept the wrong images: %s", s)
	}
	if strings.Count(s, "[image omitted") != 2 || !strings.Contains(s, "shot 1 <b>") {
		t.Fatalf("notes or text lost: %s", s)
	}
	if !strings.Contains(s, `"seed":12345678901234567890`) || !strings.Contains(s, `"temperature":0.1`) {
		t.Fatalf("numbers changed: %s", s)
	}

	// At or under the limit, or with the limit off, the body is untouched.
	for _, keep := range []int{6, 10, 0} {
		if out, dropped := trimImages(body, keep); dropped != 0 || string(out) != string(body) {
			t.Fatalf("keep %d changed the body", keep)
		}
	}
	if out, dropped := trimImages([]byte(`{"image":`), 1); dropped != 0 || string(out) != `{"image":` {
		t.Fatal("bad json changed")
	}

	// Anthropic: screenshots arrive inside tool results.
	anth := []byte(`{"model":"m","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A1"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A2"}}]}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"plain text result"}]},
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A3"}}]}]}`)
	out, dropped = trimImages(anth, 1)
	if dropped != 2 || countImages(t, out) != 1 || !strings.Contains(string(out), `"A3"`) || strings.Contains(string(out), `"A2"`) {
		t.Fatalf("anthropic: dropped %d: %s", dropped, out)
	}
}

// TestImageLimit: a vLLM server started with --limit-mm-per-prompt image=4
// rejects the whole request above four images. The gateway keeps the newest
// four, so a long agent session with many screenshots keeps working.
func TestImageLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"local-vl"}]}`)
			return
		}
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		n := 0
		for _, m := range req.Messages {
			n += len(collectImages(m["content"], nil))
		}
		seen.Store(int64(n))
		if n > 4 {
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
	client := testNode(t, ctx, Config{Bootstrap: bs, MaxImages: 4})
	for deadline := time.Now().Add(20 * time.Second); len(client.candidates("vl")) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no discovery")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var parts []string
	for i := 0; i < 7; i++ {
		parts = append(parts, fmt.Sprintf(`{"type":"image_url","image_url":{"url":"data:image/png;base64,IMG%d"}}`, i))
	}
	body := `{"model":"vl","max_tokens":20,"messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`

	if w := post(client, body); w.Code != 200 || seen.Load() != 4 {
		t.Fatalf("with --max-images 4: %d, upstream saw %d images: %s", w.Code, seen.Load(), w.Body)
	}
	client.cfg.MaxImages = 0
	if w := post(client, body); w.Code != 400 || !strings.Contains(w.Body.String(), "At most 4 image(s)") {
		t.Fatalf("with no limit the server should refuse: %d %s", w.Code, w.Body)
	}
}
