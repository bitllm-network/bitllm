package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitllm-network/bitllm/internal/models"
	"github.com/bitllm-network/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The "model" in these tests always answers with modelWords. A mock auditor
// scores an answer as that model would: the word it expects at each position
// is a sure pick (p = 0.99), any other word is very unlikely. Requests are
// greedy (the tests send temperature 0), so the share of top tokens decides.
var modelWords = strings.Fields(strings.Repeat("the quick brown fox jumps over a lazy dog while it rains ", 6))

// answerUpstream serves model and answers every chat request with words.
func answerUpstream(t *testing.T, model string, words []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
		case "/v1/chat/completions":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if _, ok := req["prompt_logprobs"]; ok {
				http.Error(w, "prompt_logprobs not supported", 400)
				return
			}
			b, _ := json.Marshal(strings.Join(words, " "))
			fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":10,"completion_tokens":%d}}`, b, len(words))
		}
	}))
}

// scoringUpstream is a vLLM-like server of model that returns prompt
// logprobs. honest=false makes a lazy auditor: every token is a sure pick.
func scoringUpstream(t *testing.T, model string, honest bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
		case "/v1/chat/completions":
			var req struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			answer := strings.Fields(req.Messages[len(req.Messages)-1].Content)
			ids := []int64{1, 2, 3} // template tokens before the answer
			lps := []any{nil, map[string]any{"2": map[string]any{"logprob": -1, "rank": 1, "decoded_token": "<u>"}},
				map[string]any{"3": map[string]any{"logprob": -1, "rank": 1, "decoded_token": "<a>"}}}
			for i, word := range answer {
				id := int64(100 + i)
				e := map[string]any{}
				want := ""
				if i < len(modelWords) {
					want = modelWords[i]
				}
				if word == want || !honest {
					e[fmt.Sprint(id)] = map[string]any{"logprob": -0.01, "rank": 1, "decoded_token": " " + word}
				} else {
					e[fmt.Sprint(id)] = map[string]any{"logprob": -12.0, "rank": 40, "decoded_token": " " + word}
					e["9"] = map[string]any{"logprob": -0.01, "rank": 1, "decoded_token": " " + want}
				}
				ids = append(ids, id)
				lps = append(lps, e)
			}
			json.NewEncoder(w).Encode(map[string]any{"prompt_token_ids": ids, "prompt_logprobs": lps,
				"choices": []any{map[string]any{"message": map[string]any{"content": "x"}}},
				"usage":   map[string]any{"prompt_tokens": len(ids), "completion_tokens": 1}})
		}
	}))
}

// auditNet starts a bootstrap node, a server of model "m" answering with
// words, an auditor whose server scores (honest or lazy), and a client with
// --audit that prefers that auditor.
func auditNet(t *testing.T, ctx context.Context, words []string, honestAuditor bool) (client, server, auditor *Node) {
	up := answerUpstream(t, "local-m", words)
	t.Cleanup(up.Close)
	sc := scoringUpstream(t, "local-m", honestAuditor)
	t.Cleanup(sc.Close)
	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	server = testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1", Models: map[string]string{"m": "local-m"}, AuditServe: true})
	auditor = testNode(t, ctx, Config{Bootstrap: bs, Upstream: sc.URL + "/v1", Models: map[string]string{"m": "local-m"}, AuditServe: true})
	client = testNode(t, ctx, Config{Bootstrap: bs, Audit: true, AuditEvery: 1, Auditors: []peer.ID{auditor.host.ID()}})
	deadline := time.Now().Add(20 * time.Second)
	for {
		al := client.alive()
		if _, ok := al[server.host.ID()]; ok && scores(al[auditor.host.ID()], "m") {
			if scores(al[server.host.ID()], "m") {
				t.Fatal("a server without prompt logprobs announced it can score")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client never saw the server and a scoring auditor: %+v", al)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return client, server, auditor
}

func waitState(t *testing.T, a *audits, p peer.ID, model, want string) *peerAudit {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		a.mu.Lock()
		pa := a.peers[auditKey{p, model}]
		var st string
		var running bool
		if pa != nil {
			st, running = pa.State, pa.running
		}
		a.mu.Unlock()
		if st == want && !running {
			return pa
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit state %q (running %v), want %q", st, running, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAuditPassesHonestServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, server, auditor := auditNet(t, ctx, modelWords, true)
	client.audits.observe(server.host.ID(), "m", []byte(`{"model":"m","temperature":0}`))
	pa := waitState(t, client.audits, server.host.ID(), "m", "pass")
	c := pa.Checks[len(pa.Checks)-1]
	if c.Stats == nil || c.Bad || c.Stats.Tokens != len(modelWords) || c.Auditor != auditor.host.ID().String() {
		t.Fatalf("check %+v stats %+v", c, c.Stats)
	}
	if got := client.candidates("m"); len(got) != 2 {
		t.Fatalf("candidates %v", got)
	}
}

func TestAuditFailsSwappedModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	other := strings.Fields(strings.Repeat("a cheaper model writes some other words here and there ", 6))
	client, server, _ := auditNet(t, ctx, other, true)
	// One auditor only: the follow-ups need another one each, so add a
	// second scoring node for them.
	sc := scoringUpstream(t, "local-m", true)
	defer sc.Close()
	second := testNode(t, ctx, Config{Bootstrap: client.cfg.Bootstrap, Upstream: sc.URL + "/v1", Models: map[string]string{"m": "local-m"}, AuditServe: true})
	_ = second
	client.cfg.Bootstrap = nil
	for deadline := time.Now().Add(20 * time.Second); !scores(client.alive()[second.host.ID()], "m"); {
		if time.Now().After(deadline) {
			t.Fatal("second auditor never seen")
		}
		time.Sleep(100 * time.Millisecond)
	}
	client.audits.observe(server.host.ID(), "m", []byte(`{"model":"m","temperature":0}`))
	pa := waitState(t, client.audits, server.host.ID(), "m", "fail")
	if len(pa.Checks) != 5 {
		t.Fatalf("%d checks, want 1 + 4 follow-ups: %+v", len(pa.Checks), pa.Checks)
	}
	for _, c := range pa.Checks {
		if !c.Bad || c.Stats.Top1 > 0.1 {
			t.Fatalf("check %+v %+v", c, c.Stats)
		}
	}
	if got := client.candidates("m"); len(got) != 2 {
		t.Fatalf("candidates after the fail: %v (want the two auditors only)", got)
	}
}

func TestAuditDropsLazyAuditor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	other := strings.Fields(strings.Repeat("a cheaper model writes some other words here and there ", 6))
	client, server, auditor := auditNet(t, ctx, other, false)
	client.audits.observe(server.host.ID(), "m", []byte(`{"model":"m","temperature":0}`))
	pa := waitState(t, client.audits, server.host.ID(), "m", "unchecked")
	if len(pa.Checks) != 1 || !strings.Contains(pa.Checks[0].Note, "control") {
		t.Fatalf("checks %+v", pa.Checks)
	}
	client.audits.mu.Lock()
	_, banned := client.audits.banned[auditor.host.ID()]
	client.audits.mu.Unlock()
	if !banned {
		t.Fatal("lazy auditor not banned")
	}
}

// A failed peer is left out of candidates, a suspect goes last.
func TestAuditRanksCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, server, auditor := auditNet(t, ctx, modelWords, true)
	a := client.audits
	a.peers[auditKey{server.host.ID(), "m"}] = &peerAudit{State: "fail", Until: time.Now().Add(time.Hour)}
	if got := client.candidates("m"); len(got) != 1 || got[0] != auditor.host.ID() {
		t.Fatalf("with a failed server: %v", got)
	}
	a.peers[auditKey{auditor.host.ID(), "m"}] = &peerAudit{State: "suspect"}
	a.peers[auditKey{server.host.ID(), "m"}] = &peerAudit{State: "pass"}
	if got := client.candidates("m"); len(got) != 2 || got[0] != server.host.ID() {
		t.Fatalf("with a suspect auditor: %v", got)
	}
	// An expired failure is checked again on next use.
	a.peers[auditKey{server.host.ID(), "m"}] = &peerAudit{State: "fail", Until: time.Now().Add(-time.Second)}
	if got := client.candidates("m"); len(got) != 2 {
		t.Fatalf("after the failure expired: %v", got)
	}
}

func TestAuditFollowUps(t *testing.T) {
	// The round logic alone, with a scorer that judges by the server.
	for _, tc := range []struct {
		name string
		bad  []bool // first check, then follow-ups
		want string
	}{
		{"pass", []bool{false}, "pass"},
		{"fail", []bool{true, true, true, false, true}, "fail"},
		{"noise", []bool{true, false, true, false, false}, "suspect"},
		{"cleared", []bool{true, false, false, false, false}, "pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := 0
			a := &audits{peers: map[auditKey]*peerAudit{}, banned: map[peer.ID]time.Time{}}
			a.checkFn = func(peer.ID, string, reqParams, map[peer.ID]bool) (auditCheck, bool) {
				b := tc.bad[i]
				i++
				return auditCheck{Bad: b, Stats: &auditStats{}, Auditor: fmt.Sprint("auditor", i)}, true
			}
			p := peer.ID("srv")
			a.peers[auditKey{p, "m"}] = &peerAudit{State: "unchecked", running: true}
			a.round(p, "m", reqParams{})
			if pa := a.peers[auditKey{p, "m"}]; pa.State != tc.want || pa.running {
				t.Fatalf("state %s running %v, want %s", pa.State, pa.running, tc.want)
			}
			if i != len(tc.bad) {
				t.Fatalf("%d checks, want %d", i, len(tc.bad))
			}
		})
	}
}

func TestAnswerText(t *testing.T) {
	s, err := answerText([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\ndata: [DONE]\n\n"), true)
	if err != nil || s != "hello" {
		t.Fatalf("stream: %q %v", s, err)
	}
	if _, err := answerText([]byte(`{"choices":[{"message":{"content":"x","reasoning_content":"hmm"}}]}`), false); err == nil {
		t.Fatal("an answer with reasoning was accepted")
	}
}

func TestScoreAnswer(t *testing.T) {
	// From a real vLLM reply (the ring), cut down: the answer " blue yellow".
	doc := `{"prompt_token_ids":[5,8295,13009],"prompt_logprobs":[null,
	 {"8295":{"logprob":-0.05,"rank":1,"decoded_token":" blue"}},
	 {"13009":{"logprob":-11.2,"rank":3,"decoded_token":" yellow"},"6726":{"logprob":-0.01,"rank":1,"decoded_token":" green"}}]}`
	st, err := scoreAnswer([]byte(doc), " blue yellow", sampler{})
	if err != nil || st.Tokens != 2 || !st.Greedy || st.Top1 != 0.5 {
		t.Fatalf("%+v %v", st, err)
	}
	// Without prompt_token_ids, the first key of each position is its token.
	noIDs := strings.Replace(doc, `"prompt_token_ids":[5,8295,13009],`, "", 1)
	if st2, err := scoreAnswer([]byte(noIDs), " blue yellow", sampler{}); err != nil || st2 != st {
		t.Fatalf("without ids: %+v %v", st2, err)
	}
	// Sampled at T=1: " yellow" (rank 3, p ~ 1e-5) is outside the top-2 the
	// position returned, so it scores as nearly impossible: z far below 0.
	sst, err := scoreAnswer([]byte(doc), " blue yellow", sampler{1, 1})
	if err != nil || sst.Greedy || sst.Z > -3 {
		t.Fatalf("sampled: %+v %v", sst, err)
	}
	if _, err := scoreAnswer([]byte(`{"choices":[]}`), "x", sampler{}); err == nil {
		t.Fatal("a reply without prompt logprobs was scored")
	}
}

func TestHashModel(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "model-1.safetensors"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "model-2.safetensors"), []byte("bbb"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644)
	h1, n, err := models.HashModel(dir)
	if err != nil || n != 2 || !models.ValidHash(h1) {
		t.Fatalf("%s %d %v", h1, n, err)
	}
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"x":1}`), 0o644)
	if h2, _, _ := models.HashModel(dir); h2 != h1 {
		t.Fatal("a config change changed the weights hash")
	}
	os.WriteFile(filepath.Join(dir, "model-1.safetensors"), []byte("aab"), 0o644)
	if h3, _, _ := models.HashModel(dir); h3 == h1 {
		t.Fatal("a weight change kept the hash")
	}
}

func TestWeightsClaim(t *testing.T) {
	h := strings.Repeat("ab", 32)
	ok := protocol.Announce{Models: []string{"m"}, Free: 1, Max: 1, Audit: []string{"m"}, Weights: map[string]string{"m": h}}
	if !protocol.ValidAnnounce(&ok) {
		t.Fatal("valid announce refused")
	}
	for name, a := range map[string]protocol.Announce{
		"audit unserved":   {Models: []string{"m"}, Free: 1, Max: 1, Audit: []string{"x"}},
		"weights unserved": {Models: []string{"m"}, Free: 1, Max: 1, Weights: map[string]string{"x": h}},
		"bad hash":         {Models: []string{"m"}, Free: 1, Max: 1, Weights: map[string]string{"m": "zz"}},
	} {
		if protocol.ValidAnnounce(&a) {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A server that states other weights than the record is skipped.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	models.Manifest["m"] = models.Info{Weights: h}
	defer delete(models.Manifest, "m")
	up := mockUpstream(t, "local-m")
	defer up.Close()
	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	good := testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1", Models: map[string]string{"m": "local-m"}, Weights: map[string]string{"m": h}})
	bad := testNode(t, ctx, Config{Bootstrap: bs, Upstream: up.URL + "/v1", Models: map[string]string{"m": "local-m"}, Weights: map[string]string{"m": strings.Repeat("cd", 32)}})
	client := testNode(t, ctx, Config{Bootstrap: bs})
	for deadline := time.Now().Add(20 * time.Second); ; {
		al := client.alive()
		if _, ok := al[bad.host.ID()]; ok {
			if _, ok := al[good.host.ID()]; ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("servers not seen")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := client.candidates("m"); len(got) != 1 || got[0] != good.host.ID() {
		t.Fatalf("candidates %v", got)
	}
}

// Without --audit-serve a node whose server could score does not offer to:
// a node that does not opt in behaves as before.
func TestAuditServeOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var scored atomic.Int32
	sc := scoringUpstream(t, "local-m", true)
	defer sc.Close()
	count := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			scored.Add(1)
		}
		resp, err := http.Post(sc.URL+r.URL.Path, "application/json", r.Body)
		if r.Method == "GET" {
			resp, err = http.Get(sc.URL + r.URL.Path)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	}))
	defer count.Close()
	boot := testNode(t, ctx, Config{})
	bs := boot.Addrs()[:1]
	srv := testNode(t, ctx, Config{Bootstrap: bs, Upstream: count.URL + "/v1", Models: map[string]string{"m": "local-m"}})
	client := testNode(t, ctx, Config{Bootstrap: bs})
	for deadline := time.Now().Add(20 * time.Second); ; {
		if a, ok := client.alive()[srv.host.ID()]; ok {
			if len(a.Audit) != 0 || len(a.Weights) != 0 {
				t.Fatalf("announce %+v", a)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never seen")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := scored.Load(); n != 0 {
		t.Fatalf("%d requests reached the server without any client asking", n)
	}
}
