package assets

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// uiHarness runs the page's own script under node against a stand-in for the
// DOM and the gateway, then runs the test's JavaScript. The script is pulled
// out of the served page, not copied, so these tests drive what ships.
const uiHarness = `
const vm = require("vm"), fs = require("fs");
const script = fs.readFileSync(process.argv[2], "utf8");
const scenario = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
const els = {};
const el = id => els[id] ||= { id, innerHTML: "", textContent: "", title: "", hidden: false, value: "",
  disabled: false, dataset: {}, classList: { toggle() {}, remove() {} },
  querySelector() { return this.innerHTML.includes("<input") ? {} : null; } };
const store = init => { const m = new Map(Object.entries(init || {}));
  return { getItem: k => m.has(k) ? m.get(k) : null, setItem: (k, v) => m.set(k, String(v)) }; };
const location = { origin: scenario.origin || "http://127.0.0.1:7778", host: "127.0.0.1:7778",
  pathname: "/", hash: "", search: scenario.search || "" };
const requests = [], replaced = [], copied = [];
let state = { status: scenario.status, models: scenario.models };
const routes = () => ({ "/status": state.status, "/v1/models": { data: state.models }, "/credits": [],
  "/account": { node: "me" }, "/members/me": scenario.members || { enabled: false } });
const ctx = {
  document: { getElementById: el }, location, console, URLSearchParams, TextEncoder, Promise, JSON, String, Math, Object, Array,
  localStorage: store(scenario.local), sessionStorage: store(scenario.session),
  history: { replaceState: (a, b, u) => replaced.push(u) },
  navigator: { clipboard: { writeText: t => { copied.push(t); return Promise.resolve(); } } },
  performance: { now: () => 0 }, setInterval() {}, setTimeout() {},
  fetch: async (u, o = {}) => { requests.push({ u, o });
    if (scenario.needKey && (o.headers || {}).Authorization !== "Bearer " + scenario.needKey)
      return { ok: false, status: 401, json: async () => ({ error: { message: "key" } }) };
    return { ok: true, status: 200, headers: { get: () => null }, json: async () => routes()[u] }; },
};
ctx.window = ctx;
vm.createContext(ctx);
vm.runInContext(script + ";globalThis.__ui = { agents, refresh, renderSnip, get model() { return model; }, set tab(k) { tab = k; }, hl };", ctx);
const ui = ctx.__ui;
const text = html => html.replace(/<[^>]*>/g, "").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&amp;/g, "&");
const click = async id => { el("models").onchange({ target: { value: id } }); await ui.refresh(); };
(async () => {
  await ui.refresh();
  const out = await (new Function("ui, el, text, click, state, copied, requests, replaced, ctx",
    "return (async () => {" + scenario.js + "})()"))(ui, el, text, click, state, copied, requests, replaced, ctx);
  console.log(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });
`

type uiScenario struct {
	Origin  string            `json:"origin,omitempty"`
	Search  string            `json:"search,omitempty"`
	Local   map[string]string `json:"local,omitempty"`
	Session map[string]string `json:"session,omitempty"`
	NeedKey string            `json:"needKey,omitempty"`
	Members map[string]any    `json:"members,omitempty"`
	Status  map[string]any    `json:"status"`
	Models  []map[string]any  `json:"models"`
	JS      string            `json:"js"`
}

// runUI returns what the scenario's JavaScript returned, decoded into out.
func runUI(t *testing.T, sc uiScenario, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if sc.Status == nil {
		sc.Status = map[string]any{}
	}
	for k, v := range map[string]any{"id": "me", "conns": []any{}, "serving": []string{}} {
		if _, ok := sc.Status[k]; !ok {
			sc.Status[k] = v
		}
	}
	if sc.Models == nil {
		sc.Models = []map[string]any{}
	}
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindSubmatch(UI)
	if m == nil {
		t.Fatal("no script block in the page")
	}
	dir := t.TempDir()
	harness, script, scenario := filepath.Join(dir, "h.js"), filepath.Join(dir, "s.js"), filepath.Join(dir, "c.json")
	b, _ := json.Marshal(sc)
	for p, c := range map[string][]byte{harness: []byte(uiHarness), script: m[1], scenario: b} {
		if err := os.WriteFile(p, c, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := exec.Command(node, harness, script, scenario).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, res)
	}
	if err := json.Unmarshal(res, out); err != nil {
		t.Fatalf("output %q: %v", res, err)
	}
}

// Names a peer could announce, none of which may do anything when pasted.
var oddNames = []string{
	"$(touch marker)", "`touch marker`", "'; touch marker; '", "a'b", `a"b`, `a\b`, "a b", "a\nb", "${HOME}", "x;touch marker", "'", "''", `\'`,
}

// The snippets for each odd name, as the page would copy them.
func snippets(t *testing.T, names []string) map[string]map[string]string {
	t.Helper()
	b, _ := json.Marshal(names)
	var out map[string]map[string]string
	runUI(t, uiScenario{JS: `const out = {}; for (const n of ` + string(b) + `) { out[n] = {};
		for (const k of Object.keys(ui.agents)) out[n][k] = ui.agents[k](n); } return out;`}, &out)
	return out
}

// Run in a real shell, the Claude Code snippet sets the name as it is and
// nothing else happens.
func TestClaudeCodeSnippetIsSafeInAShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	all := snippets(t, oddNames)
	for _, name := range oddNames {
		dir := t.TempDir()
		// claude is the snippet's last line: a function here, printing what it got.
		script := "claude() { printf '%s' \"$ANTHROPIC_MODEL\"; }\n" + all[name]["Claude Code"]
		cmd := exec.Command(sh, "-c", script)
		cmd.Dir = dir
		got, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%q: %v: %s\n%s", name, err, got, script)
			continue
		}
		if string(got) != name {
			t.Errorf("%q: the shell read %q\n%s", name, got, script)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q: pasting the snippet created %v", name, entries)
		}
	}
}

// The curl snippet hands curl valid JSON holding the name as it is.
func TestCurlSnippetIsSafeInAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sh there rereads backslashes in its own command line, before the script runs")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	all := snippets(t, oddNames)
	for _, name := range oddNames {
		dir := t.TempDir()
		script := "curl() { printf '%s' \"$3\"; }\n" + all[name]["curl"]
		cmd := exec.Command(sh, "-c", script)
		cmd.Dir = dir
		got, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%q: %v: %s\n%s", name, err, got, script)
			continue
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(got, &body); err != nil || body.Model != name {
			t.Errorf("%q: curl got %q (%v)", name, got, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q: pasting the snippet created %v", name, entries)
		}
	}
}

// The pi snippet is valid JSON holding the name as it is.
func TestPiSnippetIsValidJSON(t *testing.T) {
	all := snippets(t, oddNames)
	for _, name := range oddNames {
		_, rest, _ := strings.Cut(all[name]["pi"], "\n") // the first line is a comment
		var doc struct {
			Bitllm struct {
				Models []struct {
					ID string `json:"id"`
				} `json:"models"`
			} `json:"bitllm"`
		}
		if err := json.Unmarshal([]byte("{"+rest+"}"), &doc); err != nil {
			t.Errorf("%q: %v\n%s", name, err, rest)
		} else if doc.Bitllm.Models[0].ID != name {
			t.Errorf("%q: id %q", name, doc.Bitllm.Models[0].ID)
		}
	}
}

// What the page shows after highlighting is what it copies, on every tab.
func TestSnippetShownIsSnippetCopied(t *testing.T) {
	var out []struct{ Shown, Copied string }
	runUI(t, uiScenario{
		Status: map[string]any{"serving": []string{`mine'"<b>&.1`}},
		Models: []map[string]any{{"id": "mine.1", "nodes": 1, "canonical": true}},
		JS: `const res = []; for (const k of Object.keys(ui.agents)) { ui.tab = k; ui.renderSnip();
			el("copy").onclick(); await 0;
			res.push({ Shown: text(el("snip").innerHTML), Copied: copied[copied.length - 1] }); }
			for (const n of ["a'b", 'a"b', "<b>&amp;", "$(x)"]) for (const k of Object.keys(ui.agents)) {
				ui.tab = k; res.push({ Shown: ui.agents[k](n).split("\n").map(ui.hl).join("\n").replace(/<span[^>]*>|<\/span>/g, "")
					.replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&amp;/g, "&"), Copied: ui.agents[k](n) }); }
			return res;`,
	}, &out)
	if len(out) == 0 {
		t.Fatal("no tabs")
	}
	for _, s := range out {
		if s.Shown != s.Copied {
			t.Errorf("shown %q\ncopied %q", s.Shown, s.Copied)
		}
	}
}

type selection struct {
	Model string `json:"model"`
	Snip  string `json:"snip"`
	Rows  string `json:"rows"`
}

const readSelection = `return { model: ui.model, snip: text(el("snip").innerHTML), rows: el("models").innerHTML };`

func TestDefaultModelIsOursOrCanonical(t *testing.T) {
	odd := map[string]any{"id": "$(touch marker)", "nodes": 9}
	plain := map[string]any{"id": "a-stranger", "nodes": 9}
	canon := map[string]any{"id": "b-canon", "nodes": 1, "canonical": true}
	mine := map[string]any{"id": "c-mine", "nodes": 1}
	for name, tc := range map[string]struct {
		sc   uiScenario
		want string
	}{
		"a canonical name, past a stranger's": {uiScenario{Models: []map[string]any{odd, plain, canon}}, "b-canon"},
		"our own, past a stranger's":          {uiScenario{Models: []map[string]any{plain, mine}, Status: map[string]any{"serving": []string{"c-mine"}}}, "c-mine"},
		"nothing when nothing qualifies":      {uiScenario{Models: []map[string]any{plain}}, ""},
		"a missing field is false":            {uiScenario{Models: []map[string]any{{"id": "x", "nodes": 1, "canonical": "true"}}}, ""},
		"a saved stranger is not trusted":     {uiScenario{Local: map[string]string{"bitllm-model": "a-stranger"}, Models: []map[string]any{plain}}, ""},
		"a saved stranger loses to canonical": {uiScenario{Local: map[string]string{"bitllm-model": "a-stranger"}, Models: []map[string]any{plain, canon}}, "b-canon"},
		"a saved canonical name is kept":      {uiScenario{Local: map[string]string{"bitllm-model": "z-canon"}, Models: []map[string]any{canon, {"id": "z-canon", "nodes": 1, "canonical": true}}}, "z-canon"},
		"a saved own name is kept": {uiScenario{Local: map[string]string{"bitllm-model": "c-mine"},
			Models: []map[string]any{canon, mine}, Status: map[string]any{"serving": []string{"c-mine"}}}, "c-mine"},
		"our name that is not listed is not selected": {uiScenario{Status: map[string]any{"serving": []string{"c-mine"}}, Models: []map[string]any{plain}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			tc.sc.JS = readSelection
			var got selection
			runUI(t, tc.sc, &got)
			if got.Model != tc.want {
				t.Fatalf("selected %q, want %q", got.Model, tc.want)
			}
			if tc.want == "" && !strings.Contains(got.Snip, "MODEL") {
				t.Fatalf("no placeholder in the snippet: %q", got.Snip)
			}
			if tc.want != "" && !strings.Contains(got.Snip, tc.want) {
				t.Fatalf("snippet without %q: %q", tc.want, got.Snip)
			}
			if strings.Contains(got.Snip, "touch marker") {
				t.Fatalf("snippet holds a stranger's name: %q", got.Snip)
			}
		})
	}
}

// A click is a choice: it holds while the model is listed, and is not trusted
// again on its own once it is gone.
func TestClickedModelIsAChoice(t *testing.T) {
	var got []string
	runUI(t, uiScenario{
		Models: []map[string]any{{"id": "a-stranger", "nodes": 1}, {"id": "b-canon", "nodes": 1, "canonical": true}},
		JS: `const res = [ui.model];
			await click("a-stranger"); res.push(ui.model, ctx.localStorage.getItem("bitllm-model"));
			await ui.refresh(); res.push(ui.model);
			state.models = state.models.filter(m => m.id !== "a-stranger"); await ui.refresh(); res.push(ui.model);
			state.models = [{ id: "a-stranger", nodes: 1 }, { id: "b-canon", nodes: 1, canonical: true }]; await ui.refresh(); res.push(ui.model);
			return res;`,
	}, &got)
	want := []string{"b-canon", "a-stranger", "a-stranger", "a-stranger", "b-canon", "b-canon"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Rows are limited to the allow-list and say what is unverified.
func TestModelRows(t *testing.T) {
	var got selection
	runUI(t, uiScenario{
		Status: map[string]any{"serving": []string{"mine"}},
		Models: []map[string]any{
			{"id": "$(touch marker)", "nodes": 1}, {"id": "-rf", "nodes": 1}, {"id": "a b", "nodes": 1}, {"id": "a\nb", "nodes": 1},
			{"id": "", "nodes": 1}, {"id": 7, "nodes": 1}, {"id": "a'b", "nodes": 1},
			{"id": "Org/model-1.5_x:q4@v2+x=y", "nodes": 2}, {"id": "canon", "nodes": 1, "canonical": true}, {"id": "mine", "nodes": 1},
		},
		JS: readSelection,
	}, &got)
	if n := strings.Count(got.Rows, `class="model"`); n != 3 {
		t.Fatalf("%d rows, want 3: %s", n, got.Rows)
	}
	if n := strings.Count(got.Rows, "unverified name"); n != 1 {
		t.Fatalf("%d rows marked unverified, want 1: %s", n, got.Rows)
	}
	for _, want := range []string{"Org/model-1.5_x:q4@v2+x=y", "canon", "mine", ">yours<"} {
		if !strings.Contains(got.Rows, want) {
			t.Errorf("no %q in %s", want, got.Rows)
		}
	}
}

func TestAnnounceErrorAndUpdateNotes(t *testing.T) {
	var got struct {
		Err     string
		Hidden  bool
		Version string
		Clean   string
		Bare    bool
	}
	runUI(t, uiScenario{
		Status: map[string]any{"version": "v0.5.0", "announce_error": "<b>too many</b>", "update": map[string]any{"signature": "ok", "pending_restart": true}},
		JS: `const res = { Err: el("share-err").textContent, Hidden: el("share-err").hidden, Version: el("version").textContent };
			state.status = { ...state.status, announce_error: undefined, update: undefined }; await ui.refresh();
			res.Clean = el("version").textContent; res.Bare = el("share-err").hidden; return res;`,
	}, &got)
	if got.Hidden || !strings.Contains(got.Err, "<b>too many</b>") {
		t.Errorf("announce error: hidden %v, %q", got.Hidden, got.Err)
	}
	if got.Version != "v0.5.0 / signature ok / restart pending" {
		t.Errorf("version line %q", got.Version)
	}
	if !got.Bare || got.Clean != "v0.5.0" {
		t.Errorf("with nothing to say: hidden %v, %q", got.Bare, got.Clean)
	}
}

// The copy under credits says what the table is.
func TestCreditsCopy(t *testing.T) {
	for _, old := range []string{"same table on every node", "Every node computes the same table"} {
		if bytes.Contains(UI, []byte(old)) {
			t.Errorf("the page still says %q", old)
		}
	}
	if !bytes.Contains(UI, []byte("self-reported")) {
		t.Error("the page does not say receipts are self-reported")
	}
	if bytes.Contains(page, []byte("—")) {
		t.Error("an em dash in the page")
	}
}

// With ?key= the page keeps the key for this tab, cleans the URL, sends the key
// on every request, and puts it in the snippets, where a client needs it.
func TestGatewayKeyInThePage(t *testing.T) {
	var got struct {
		Saved, Curl, Claude, Pi string
		Replaced                []string
		Sent, All               int
	}
	const key = "k'ey$1"
	runUI(t, uiScenario{
		Search: "?a=1&key=k%27ey%241", NeedKey: key,
		Status: map[string]any{"version": "v0.5.0"},
		JS: `return { Saved: ctx.sessionStorage.getItem("bitllm-key"), Replaced: replaced, All: requests.length,
			Sent: requests.filter(r => r.o.headers && r.o.headers.Authorization === "Bearer k'ey$1").length,
			Curl: ui.agents["curl"]("m"), Claude: ui.agents["Claude Code"]("m"), Pi: ui.agents["pi"]("m") };`,
	}, &got)
	if got.Saved != key {
		t.Errorf("sessionStorage holds %q", got.Saved)
	}
	if len(got.Replaced) != 1 || got.Replaced[0] != "/?a=1" {
		t.Errorf("URL became %v", got.Replaced)
	}
	if got.All == 0 || got.Sent != got.All {
		t.Errorf("%d of %d requests carried the key", got.Sent, got.All)
	}
	if !strings.Contains(got.Claude, `ANTHROPIC_API_KEY='k'\''ey$1'`) {
		t.Errorf("Claude Code snippet: %s", got.Claude)
	}
	if !strings.Contains(got.Curl, `-H 'Authorization: Bearer k'\''ey$1'`) {
		t.Errorf("curl snippet: %s", got.Curl)
	}
	if !strings.Contains(got.Pi, `"apiKey": "k'ey$1"`) {
		t.Errorf("pi snippet: %s", got.Pi)
	}
}

// A reload finds the key in sessionStorage, and a page without one says so.
func TestGatewayKeyAcrossLoads(t *testing.T) {
	var got struct {
		Sent bool
		Net  string
	}
	runUI(t, uiScenario{Session: map[string]string{"bitllm-key": "k"}, NeedKey: "k",
		JS: `return { Sent: requests.every(r => r.o.headers.Authorization === "Bearer k"), Net: el("net-text").textContent };`}, &got)
	if !got.Sent || got.Net != "No peers yet" {
		t.Errorf("with a saved key: %+v", got)
	}
	runUI(t, uiScenario{NeedKey: "k", JS: `return { Sent: false, Net: el("net-text").textContent };`}, &got)
	if !strings.Contains(got.Net, "?key=") {
		t.Errorf("without a key, the page says %q", got.Net)
	}
}

// Without a key the page sends no header and its snippets stay as they were.
func TestNoGatewayKeyInThePage(t *testing.T) {
	var got struct {
		Headers bool
		Curl    string
		OpenAI  string
	}
	runUI(t, uiScenario{JS: `return { Headers: requests.some(r => r.o.headers), Curl: ui.agents["curl"]("m"), OpenAI: ui.agents["OpenAI-compatible"]("m") };`}, &got)
	if got.Headers {
		t.Error("a request carried headers")
	}
	if strings.Contains(got.Curl, "Authorization") || !strings.Contains(got.OpenAI, "bitllm  (any value)") {
		t.Errorf("snippets: %q %q", got.Curl, got.OpenAI)
	}
}

// A contributor line promises what access rule 3 says and no more.
func TestContributorCopy(t *testing.T) {
	var got string
	runUI(t, uiScenario{
		Members: map[string]any{"enabled": true, "account": "0xabc", "member": true, "contributor": true, "invites_limit": 10, "invites_used": 1, "members": 5},
		JS:      `return el("access-text").innerHTML;`,
	}, &got)
	for _, want := range []string{"10 invites instead of 3", "no quota on a server that asked your node for a job in the last 7 days"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	if strings.Contains(got, "never limited") {
		t.Errorf("promises too much: %q", got)
	}
}
