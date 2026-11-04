package main

// Spot checks of which model a peer really serves (--audit).
//
// After some of our real requests to a peer, the gateway sends that peer a
// canary: an ordinary chat request from a built-in pool, with the sampling
// settings our real requests use. Then it asks an auditor, another node that
// serves the same model and whose server returns prompt logprobs, to score the
// answer token by token (teacher forcing). Under greedy decoding an honest
// answer takes the model's top token nearly everywhere; under sampling its
// tokens are as likely as the model's own samples (a z score near 0). Another
// model's answer falls short on both. The auditor gets only the text: it is
// not told which peer wrote it.
//
// One bad score makes a peer a suspect; it is then checked four more times,
// each with another auditor, and fails only if three of those are bad too.
// A failed peer gets no requests for that model for failFor. The verdict
// stays on this node: nothing is gossiped.

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// DefaultAuditors are tried first as auditors. They are run by the BitLLM
// maintainers (the omarchy node, which scores deepseek-v4.1-flash-1017-heretic
// on the 4-node ring). --auditor replaces them, --auditor none drops them; any
// other node that announces it can score a model is used after them.
var DefaultAuditors = []string{"12D3KooWSCjSmNqem7jVEwc3scunEjpTqSZPgPdhMx6VbZ5piETF"}

//go:embed canaries.txt
var canariesTxt string

var canaries = func() []string {
	var out []string
	for _, l := range strings.Split(canariesTxt, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}()

const (
	canaryTokens = 256 // max_tokens of a canary
	scoreTopK    = 8   // prompt logprobs asked of an auditor per position
	minScored    = 40  // answers with fewer tokens are not judged
	auditsPerHr  = 6   // rounds per peer and model per hour, at most
	followUps    = 4   // checks of a suspect, each with another auditor
	failBad      = 3   // bad follow-ups that make a suspect fail
	failFor      = 24 * time.Hour
	auditorBan   = 24 * time.Hour // an auditor that passed a shuffled answer
	controlEvery = 4              // one check in this many also scores a shuffled answer

	// When a score is bad. From docs/audit.md (DeepSeek V4.1 on the ring as
	// auditor, 40 answers per arm): honest greedy answers had top1 = 1.000
	// in 40 of 40, GLM 5.3 Flash at most 0.926; honest sampled answers had
	// z >= -6.9, GLM z <= -19.0. Provisional until the cross-engine run.
	minTop1 = 0.93 // greedy: share of tokens that are the auditor's top token
	minZ    = -12  // sampled: log-likelihood z score under the sampling distribution
)

// sampler is how a canary was sampled: temperature 0 is greedy.
type sampler struct {
	T, TopP float64
}

// canarySampler is the sampling a canary asks for: the model's fixed one if
// its servers ignore the client's; else greedy every other time, since only
// greedy answers show a swapped quant (docs/audit.md); else that of our
// latest real request (temperature 1 if it named none, since a server's
// default is unknown).
func canarySampler(model string, rp reqParams) sampler {
	if f := manifest[model].Sampling; f != nil {
		return sampler{f.Temperature, f.TopP}
	}
	if rand.IntN(2) == 0 {
		return sampler{0, 1}
	}
	sp := sampler{1, 1}
	if rp.Temperature != nil {
		sp.T = max(*rp.Temperature, 0)
	}
	if rp.TopP != nil && *rp.TopP > 0 && *rp.TopP <= 1 {
		sp.TopP = *rp.TopP
	}
	return sp
}

// auditStats is an auditor's view of one answer.
type auditStats struct {
	Tokens int     `json:"tokens"`
	Greedy bool    `json:"greedy"`
	Top1   float64 `json:"top1"` // share of tokens that are the auditor's top token
	Z      float64 `json:"z"`    // sum of (log p + entropy) over its standard deviation; ~N(0,1) if honest
}

func (s auditStats) bad() bool {
	if s.Greedy {
		return s.Top1 < minTop1
	}
	return s.Z < minZ
}

type auditCheck struct {
	Time    time.Time   `json:"time"`
	Auditor string      `json:"auditor"`
	Stats   *auditStats `json:"stats,omitempty"`
	Bad     bool        `json:"bad"`
	Note    string      `json:"note,omitempty"`
	AskMs   int64       `json:"canary_ms,omitempty"` // the canary, end to end
	ScoreMs int64       `json:"score_ms,omitempty"`  // the auditor's scoring, end to end
}

// peerAudit is what we know about one peer serving one model.
type peerAudit struct {
	Peer   string       `json:"peer"`
	Model  string       `json:"model"`
	State  string       `json:"state"` // unchecked, pass, suspect, fail
	Until  time.Time    `json:"until,omitzero"`
	Checks []auditCheck `json:"checks"` // newest last

	real    int // real requests since the last round
	running bool
	rounds  []time.Time
}

type auditKey struct {
	p     peer.ID
	model string
}

// reqParams are the sampling settings of our latest real request for a model,
// which canaries copy so that they look like our other traffic.
type reqParams struct {
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	Stream      bool     `json:"stream"`
}

type audits struct {
	n       *Node
	every   int // real requests between rounds
	seeds   []peer.ID
	mu      sync.Mutex
	peers   map[auditKey]*peerAudit
	banned  map[peer.ID]time.Time // auditors caught passing a shuffled answer
	checks  int
	scoreFn func(ctx context.Context, a peer.ID, model, prompt, answer string, sp sampler) (auditStats, error) // tests
	checkFn func(p peer.ID, model string, rp reqParams, used map[peer.ID]bool) (auditCheck, bool)
}

const maxAudited = 1024 // (peer, model) pairs remembered

func newAudits(n *Node, every int, seeds []peer.ID) *audits {
	a := &audits{n: n, every: max(every, 1), seeds: seeds, peers: map[auditKey]*peerAudit{}, banned: map[peer.ID]time.Time{}}
	a.scoreFn, a.checkFn = a.score, a.check
	return a
}

// observe is called after a real request to p for model was answered.
func (a *audits) observe(p peer.ID, model string, body []byte) {
	var rp reqParams
	json.Unmarshal(body, &rp)
	a.mu.Lock()
	defer a.mu.Unlock()
	k := auditKey{p, model}
	pa := a.peers[k]
	if pa == nil {
		if len(a.peers) >= maxAudited {
			return
		}
		pa = &peerAudit{Peer: p.String(), Model: model, State: "unchecked"}
		a.peers[k] = pa
	}
	pa.real++
	now := time.Now()
	if pa.State == "fail" && now.After(pa.Until) {
		pa.State = "unchecked"
	}
	due := pa.State == "unchecked" || pa.real >= a.every
	recent := pa.rounds[:0]
	for _, t := range pa.rounds {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	pa.rounds = recent
	if !due || pa.running || len(pa.rounds) >= auditsPerHr {
		return
	}
	pa.running, pa.real = true, 0
	pa.rounds = append(pa.rounds, now)
	go a.round(p, model, rp)
}

// round checks p once, and four more times if that check is bad. Follow-ups
// go to other auditors while there are any. A suspect fails only on bad
// scores from two auditors, or from one we were told to trust (--auditor), so
// that one lying auditor cannot shut out an honest server on its own.
func (a *audits) round(p peer.ID, model string, rp reqParams) {
	defer func() {
		a.mu.Lock()
		a.peers[auditKey{p, model}].running = false
		a.mu.Unlock()
	}()
	used := map[peer.ID]bool{}
	c, ok := a.checkFn(p, model, rp, used)
	if !ok {
		return
	}
	if !c.Bad {
		a.setState(p, model, "pass", 0)
		return
	}
	a.setState(p, model, "suspect", 0)
	log.Printf("audit: %s answered a %s canary unlike the model (greedy %v, top1 %.3f, z %.1f over %d tokens); checking again",
		p, model, c.Stats.Greedy, c.Stats.Top1, c.Stats.Z, c.Stats.Tokens)
	bad, done := 0, 0
	blamers := map[string]bool{c.Auditor: true}
	for i := 0; i < followUps*2 && done < followUps; i++ {
		c, ok := a.checkFn(p, model, rp, used)
		if !ok {
			continue
		}
		done++
		if c.Bad {
			bad++
			blamers[c.Auditor] = true
		}
	}
	trusted := false
	for _, s := range a.seeds {
		trusted = trusted || blamers[s.String()]
	}
	switch {
	case bad >= failBad && (len(blamers) >= 2 || trusted):
		a.setState(p, model, "fail", failFor)
		log.Printf("audit: %s FAILED for %s (%d of %d follow-ups bad, %d auditor(s)); not using it for this model for %v",
			p, model, bad, done, len(blamers), failFor)
	case done == followUps && bad == 0:
		a.setState(p, model, "pass", 0)
	}
}

func (a *audits) setState(p peer.ID, model, state string, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pa := a.peers[auditKey{p, model}]
	pa.State = state
	pa.Until = time.Time{}
	if d > 0 {
		pa.Until = time.Now().Add(d)
	}
}

func (a *audits) record(p peer.ID, model string, c auditCheck) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pa := a.peers[auditKey{p, model}]
	pa.Checks = append(pa.Checks, c)
	if len(pa.Checks) > 20 {
		pa.Checks = pa.Checks[len(pa.Checks)-20:]
	}
}

// check sends p one canary and has an auditor score the answer: one not in
// used if there is one, else any that fits.
// ok is false when there was nothing to judge (no auditor, a short answer...).
func (a *audits) check(p peer.ID, model string, rp reqParams, used map[peer.ID]bool) (auditCheck, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	auditor, ok := a.pickAuditor(model, p, used)
	if !ok {
		if auditor, ok = a.pickAuditor(model, p, nil); !ok {
			return auditCheck{}, false
		}
	}
	used[auditor] = true
	prompt := canaries[rand.IntN(len(canaries))]
	t0 := time.Now()
	sp := canarySampler(model, rp)
	answer, err := a.ask(ctx, p, model, prompt, rp.Stream, sp)
	c := auditCheck{Time: t0, Auditor: auditor.String(), AskMs: time.Since(t0).Milliseconds()}
	if err != nil {
		c.Note = "canary: " + err.Error()
		a.record(p, model, c)
		return c, false
	}
	t1 := time.Now()
	st, err := a.scoreFn(ctx, auditor, model, prompt, answer, sp)
	c.ScoreMs = time.Since(t1).Milliseconds()
	if err != nil {
		c.Note = "score: " + err.Error()
		a.record(p, model, c)
		return c, false
	}
	if st.Tokens < minScored {
		c.Note = "answer too short to judge"
		a.record(p, model, c)
		return c, false
	}
	// Now and then also hand the auditor the same answer with its words
	// shuffled. An auditor that really scores must call that bad; one that
	// passes it is not scoring, and is dropped along with its verdict.
	a.mu.Lock()
	a.checks++
	control := a.checks%controlEvery == 1
	a.mu.Unlock()
	if control {
		if cs, err := a.scoreFn(ctx, auditor, model, prompt, shuffleWords(answer), sp); err == nil && !cs.bad() {
			a.mu.Lock()
			a.banned[auditor] = time.Now().Add(auditorBan)
			a.mu.Unlock()
			log.Printf("audit: auditor %s passed a shuffled answer; not using it for %v", auditor, auditorBan)
			c.Note = "auditor failed its control; verdict dropped"
			a.record(p, model, c)
			return c, false
		}
	}
	c.Stats, c.Bad = &st, st.bad()
	a.record(p, model, c)
	return c, true
}

// shuffleWords returns s with its words in random order.
func shuffleWords(s string) string {
	w := strings.Fields(s)
	rand.Shuffle(len(w), func(i, j int) { w[i], w[j] = w[j], w[i] })
	return strings.Join(w, " ")
}

// pickAuditor chooses who scores an answer of server: a seed auditor first,
// then any other node that announces it can score model. Never the server
// itself, a node of the same owner, one on the same /24 (or /48), a banned
// auditor, or one in used.
func (a *audits) pickAuditor(model string, server peer.ID, used map[peer.ID]bool) (peer.ID, bool) {
	alive := a.n.alive()
	srvNets := a.n.peerNets(server)
	srvOwner := ""
	if c, ok := a.n.claims.get(server.String()); ok {
		srvOwner = c.Owner
	}
	a.mu.Lock()
	banned := map[peer.ID]bool{}
	for p, t := range a.banned {
		if time.Now().Before(t) {
			banned[p] = true
		}
	}
	a.mu.Unlock()
	fit := func(p peer.ID) bool {
		ann, ok := alive[p]
		if !ok || p == server || used[p] || banned[p] || !scores(ann, model) {
			return false
		}
		if c, ok := a.n.claims.get(p.String()); ok && srvOwner != "" && c.Owner == srvOwner {
			return false
		}
		for nw := range a.n.peerNets(p) {
			if srvNets[nw] {
				return false
			}
		}
		return true
	}
	for _, p := range a.seeds {
		if fit(p) {
			return p, true
		}
	}
	var rest []peer.ID
	for p := range alive {
		if fit(p) {
			rest = append(rest, p)
		}
	}
	if len(rest) == 0 {
		return "", false
	}
	return rest[rand.IntN(len(rest))], true
}

func scores(a Announce, model string) bool {
	for _, m := range a.Audit {
		if m == model {
			return true
		}
	}
	return false
}

// peerNets lists the /24 (IPv4) or /48 (IPv6) networks of p's public
// addresses, so an auditor next door to the server is not chosen.
func (n *Node) peerNets(p peer.ID) map[string]bool {
	out := map[string]bool{}
	for _, addr := range n.host.Peerstore().Addrs(p) {
		if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err == nil {
			continue
		}
		ip, err := manet.ToIP(addr)
		if err != nil || !manet.IsPublicAddr(addr) {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			out[v4.Mask(net.CIDRMask(24, 32)).String()] = true
		} else {
			out[ip.Mask(net.CIDRMask(48, 128)).String()] = true
		}
	}
	return out
}

// canaryBody is the chat request for a canary, or for scoring one when
// answer is set.
func canaryBody(model, prompt, answer string, stream bool, sp sampler) []byte {
	msgs := []map[string]string{{"role": "user", "content": prompt}}
	body := map[string]any{"model": model}
	for k, v := range manifest[model].Extra {
		body[k] = v
	}
	if answer == "" {
		body["max_tokens"] = canaryTokens
		body["stream"] = stream
		body["temperature"] = sp.T
		if sp.TopP < 1 {
			body["top_p"] = sp.TopP
		}
	} else {
		msgs = append(msgs, map[string]string{"role": "assistant", "content": answer})
		body["add_generation_prompt"] = false
		body["continue_final_message"] = true
		body["prompt_logprobs"] = scoreTopK
		body["return_token_ids"] = true // vLLM sends prompt_token_ids only when asked
		body["max_tokens"] = 1
		body["temperature"] = 0
	}
	body["messages"] = msgs
	b, _ := json.Marshal(body)
	return b
}

// ask sends p a canary and returns the answer's text. An answer that carries
// reasoning is refused: the auditor cannot rebuild the hidden part.
func (a *audits) ask(ctx context.Context, p peer.ID, model, prompt string, stream bool, sp sampler) (string, error) {
	w := httptest.NewRecorder()
	if _, err := a.n.infer(ctx, p, "/chat/completions", canaryBody(model, prompt, "", stream, sp), stream, w, time.Minute); err != nil {
		return "", err
	}
	return answerText(w.Body.Bytes(), stream)
}

type answerDelta struct {
	Content   string `json:"content"`
	Reasoning string `json:"reasoning_content"`
	Reason2   string `json:"reasoning"`
}

func answerText(b []byte, stream bool) (string, error) {
	var text strings.Builder
	reasoning := false
	add := func(d answerDelta) {
		text.WriteString(d.Content)
		reasoning = reasoning || d.Reasoning != "" || d.Reason2 != ""
	}
	if !stream {
		var doc struct {
			Choices []struct {
				Message answerDelta `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(b, &doc); err != nil || len(doc.Choices) == 0 {
			return "", auditErr("no answer in the response")
		}
		add(doc.Choices[0].Message)
	} else {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			data, ok := bytes.CutPrefix(bytes.TrimSpace(sc.Bytes()), []byte("data:"))
			if !ok {
				continue
			}
			var ev struct {
				Choices []struct {
					Delta answerDelta `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal(bytes.TrimSpace(data), &ev) == nil && len(ev.Choices) > 0 {
				add(ev.Choices[0].Delta)
			}
		}
	}
	if reasoning {
		return "", auditErr("the answer carries reasoning, which cannot be scored")
	}
	return text.String(), nil
}

type auditErr string

func (e auditErr) Error() string { return string(e) }

// score has auditor score answer as the reply to prompt.
func (a *audits) score(ctx context.Context, auditor peer.ID, model, prompt, answer string, sp sampler) (auditStats, error) {
	w := httptest.NewRecorder()
	if _, err := a.n.infer(ctx, auditor, "/chat/completions", canaryBody(model, prompt, answer, false, sampler{}), false, w, time.Minute); err != nil {
		return auditStats{}, err
	}
	return scoreAnswer(w.Body.Bytes(), answer, sp)
}

// scoreDoc is the part of a vLLM response with prompt_logprobs we read. Each
// position maps token ids to {logprob, rank}: the prompt's own token first,
// then the top ones.
type scoreDoc struct {
	IDs      []int64           `json:"prompt_token_ids"`
	Logprobs []json.RawMessage `json:"prompt_logprobs"`
}

type scoredTokenDoc struct {
	Logprob float64 `json:"logprob"`
	Rank    int     `json:"rank"`
	Decoded string  `json:"decoded_token"`
}

// firstKey is the first key of a JSON object: the scored token, for a vLLM
// that does not return prompt_token_ids.
func firstKey(raw json.RawMessage) string {
	d := json.NewDecoder(bytes.NewReader(raw))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	t, _ := d.Token()
	k, _ := t.(string)
	return k
}

// scoreAnswer reads the auditor's logprobs for the last tokens of the prompt,
// as many as it takes to spell answer, and sums them up.
func scoreAnswer(b []byte, answer string, sp sampler) (auditStats, error) {
	var d scoreDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return auditStats{}, auditErr("auditor reply: " + err.Error())
	}
	if len(d.Logprobs) == 0 || (d.IDs != nil && len(d.IDs) != len(d.Logprobs)) {
		return auditStats{}, auditErr("auditor returned no prompt logprobs")
	}
	st := auditStats{Greedy: sp.T < 0.01}
	top1 := 0
	var sum, variance float64
	for i, got := len(d.Logprobs)-1, 0; i > 0 && got < len(answer); i-- {
		var pos map[string]scoredTokenDoc
		if json.Unmarshal(d.Logprobs[i], &pos) != nil {
			return auditStats{}, auditErr("auditor reply: bad prompt logprobs")
		}
		id := firstKey(d.Logprobs[i])
		if d.IDs != nil {
			id = itoa(d.IDs[i])
		}
		tok, ok := pos[id]
		if !ok {
			return auditStats{}, auditErr("auditor reply lacks a scored token")
		}
		got += len(tok.Decoded)
		st.Tokens++
		if tok.Rank == 1 {
			top1++
		}
		if !st.Greedy {
			lp, h, v := sampled(pos, tok, sp)
			sum += lp + h
			variance += v
		}
	}
	if st.Tokens > 0 {
		st.Top1 = float64(top1) / float64(st.Tokens)
	}
	if !st.Greedy {
		st.Z = sum / math.Sqrt(max(variance, 1e-9))
	}
	return st, nil
}

// sampled returns, for one position, the log probability of the answer's
// token under the sampling distribution (the auditor's top tokens at
// temperature T, cut to the top-p nucleus), that distribution's entropy, and
// the variance of its log probability.
func sampled(pos map[string]scoredTokenDoc, tok scoredTokenDoc, sp sampler) (lp, h, v float64) {
	var top []float64
	for _, e := range pos {
		if e.Rank >= 1 && e.Rank <= scoreTopK {
			top = append(top, e.Logprob)
		}
	}
	if len(top) == 0 {
		return math.Log(1e-6), 0, 0
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(top)))
	ws := make([]float64, len(top))
	tot := 0.0
	for i, l := range top {
		ws[i] = math.Exp((l - top[0]) / sp.T)
		tot += ws[i]
	}
	keep, mass := 0, 0.0
	for keep < len(ws) {
		mass += ws[keep] / tot
		keep++
		if mass >= sp.TopP {
			break
		}
	}
	for i := 0; i < keep; i++ {
		q := ws[i] / tot / mass
		h -= q * math.Log(q)
		v += q * math.Log(q) * math.Log(q)
	}
	v -= h * h
	lp = math.Log(1e-6)
	if tok.Rank >= 1 && tok.Rank <= keep {
		lp = math.Log(math.Exp((tok.Logprob-top[0])/sp.T) / tot / mass)
	}
	return lp, h, v
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

// excluded reports whether p failed its audit for model; demoted whether it
// is a suspect.
func (a *audits) rank(p peer.ID, model string) (excluded, demoted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pa := a.peers[auditKey{p, model}]
	if pa == nil {
		return false, false
	}
	if pa.State == "fail" {
		return time.Now().Before(pa.Until), false
	}
	return false, pa.State == "suspect"
}

// probeScoring finds which of our models our server can score, by asking it
// to score a short answer. Those are announced, so others can use us as an
// auditor.
func (n *Node) probeScoring(ctx context.Context) []string {
	var out []string
	for public := range n.cfg.Models {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, _, err := n.callUpstream(ctx, "/chat/completions",
			canaryBody(public, "Say hello.", "Hello! How can I help you today?", false, sampler{}))
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if st, err := scoreAnswer(b, "Hello! How can I help you today?", sampler{}); err == nil && st.Tokens > 0 {
				out = append(out, public)
			}
		}
		cancel()
	}
	sort.Strings(out)
	return out
}

// auditList is GET /audits: what this node has found about its peers.
func (n *Node) auditList(w http.ResponseWriter, r *http.Request) {
	out := []peerAudit{}
	if n.audits != nil {
		n.audits.mu.Lock()
		for _, pa := range n.audits.peers {
			c := *pa
			c.Checks = append([]auditCheck(nil), pa.Checks...)
			out = append(out, c)
		}
		n.audits.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Peer < out[j].Peer
	})
	resp := map[string]any{"enabled": n.audits != nil, "peers": out}
	if n.audits != nil {
		pref := []map[string]any{}
		for _, p := range n.audits.seeds {
			pref = append(pref, map[string]any{"peer": p.String(), "seed": isSeed(p)})
		}
		resp["preferred_auditors"] = pref
		resp["note"] = "seed auditors are run by the BitLLM maintainers; replace them with --auditor PEER_ID or drop them with --auditor none"
	}
	json.NewEncoder(w).Encode(resp)
}

// isSeed reports whether p is one of the maintainers' seed auditors.
func isSeed(p peer.ID) bool {
	for _, s := range DefaultAuditors {
		if s == p.String() {
			return true
		}
	}
	return false
}
