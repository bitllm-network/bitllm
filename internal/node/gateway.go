package node

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/bitllm-org/bitllm/internal/account"
	"github.com/bitllm-org/bitllm/internal/assets"
	"github.com/bitllm-org/bitllm/internal/models"
	"github.com/bitllm-org/bitllm/internal/protocol"
	"github.com/libp2p/go-libp2p/core/peer"
)

// busyWait is how long a request queues when every peer serving its model is
// busy. The waiting itself happens on one stream, in the server's queue; this
// is the longest the gateway keeps trying.
var busyWait = 60 * time.Second

// busyBackoff is how long the gateway waits before trying again when every
// peer refused at once (its queue full, or a node too old to queue at all).
var busyBackoff = time.Second

// peerWait is the longest one peer may hold the request while another
// candidate is still untried. Without it a deep queue on the first peer
// hides an idle second one for the whole busyWait.
var peerWait = 15 * time.Second

// New does not wait for the network, so right after a start the gateway may
// not have heard of any peer yet. A request for a peer model made within
// startupWindow of the start waits up to startupWait for a first candidate,
// looking again every startupPoll.
var (
	startupWindow = 30 * time.Second
	startupWait   = 15 * time.Second
	startupPoll   = 250 * time.Millisecond
)

// Gateway is the local OpenAI-compatible endpoint. Point any agent at it; it
// answers from our own upstream when we serve the model, otherwise from a peer.
func (n *Node) Gateway() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", n.listModels)
	mux.HandleFunc("POST /v1/chat/completions", n.complete("/chat/completions"))
	mux.HandleFunc("POST /v1/completions", n.complete("/completions"))
	mux.HandleFunc("POST /v1/messages", n.complete("/messages"))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(assets.UI)
	})
	mux.HandleFunc("GET /status", n.status)
	mux.HandleFunc("GET /queue", n.queueView)
	mux.HandleFunc("GET /network", n.network)
	mux.HandleFunc("GET /logs", n.logs)
	mux.HandleFunc("GET /audits", n.auditList)
	mux.HandleFunc("GET /account", n.account)
	mux.HandleFunc("POST /account/message", n.accountMessage)
	mux.HandleFunc("POST /account/link", n.accountLink)
	mux.HandleFunc("GET /members/me", n.membersMe)
	mux.HandleFunc("GET /members", n.membersList)
	mux.HandleFunc("POST /members", n.membersPost)
	mux.HandleFunc("GET /credits", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(n.creditsTable())
	})
	return sameOrigin(requireKey(n.cfg.GatewayKey, mux))
}

// sameOrigin refuses requests from other web pages. The gateway listens on
// localhost without auth, so without this any website you visit could spend
// your credits or relink your node through your browser. Checking Origin
// alone is not enough: with DNS rebinding a page at evil.example resolves its
// own name to 127.0.0.1, and then Origin and Host agree. So the Host must
// also be an IP literal or localhost, which a rebinding page cannot send.
func sameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			jsonErr(w, 403, "use http://127.0.0.1 or http://localhost to reach the gateway")
			return
		}
		if o := r.Header.Get("Origin"); r.Method != "GET" && o != "" &&
			o != "http://"+r.Host && o != "https://"+r.Host {
			jsonErr(w, 403, "cross-origin request refused")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// requireKey asks every request for the gateway key, when there is one. Only
// GET / goes without: that page holds no data, and it is how a browser gets the
// key to attach to its own requests. The key is hashed first so the comparison
// takes the same time whatever the length of what was sent.
func requireKey(key string, h http.Handler) http.Handler {
	if key == "" {
		return h
	}
	want := sha256.Sum256([]byte(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != "GET" || r.URL.Path != "/") && !carriesKey(r, want) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bitllm"`)
			jsonErr(w, 401, "this gateway has a key: send it as Authorization: Bearer or x-api-key")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// carriesKey checks the two headers clients send their API key in: OpenAI
// clients use Authorization, Claude Code x-api-key. Both are always compared.
func carriesKey(r *http.Request, want [sha256.Size]byte) bool {
	bearer := ""
	if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		bearer = strings.TrimSpace(token)
	}
	a, b := sha256.Sum256([]byte(bearer)), sha256.Sum256([]byte(r.Header.Get("x-api-key")))
	return subtle.ConstantTimeCompare(a[:], want[:])|subtle.ConstantTimeCompare(b[:], want[:]) == 1
}

func localHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}

// A request line to a peer must fit maxRequest, the limit the peer reads it
// under, so the gateway keeps room for what goes around the body: the job id,
// the path, wait_ms, the field names and the newline.
const (
	inferWrapper   = 256
	maxGatewayBody = maxRequest - inferWrapper
)

var msgTooLarge = fmt.Sprintf("request body is over this gateway's limit of %d bytes (about %d MiB)", maxGatewayBody, maxRequest>>20)

// retryAfter is the Retry-After of a 429, in seconds. By then the gateway has
// already waited out busyWait, so the client need not hold back for long.
const retryAfter = "10"

// gatewayErr answers an error from a completions route in the shape of the API
// the client is speaking.
func gatewayErr(w http.ResponseWriter, path string, status int, msg string) {
	if status == 429 {
		w.Header().Set("Retry-After", retryAfter)
	}
	if path != "/messages" {
		jsonErr(w, status, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": anthropicErrType(status), "message": msg},
	})
}

// anthropicErrType is the error type Anthropic's API uses for a status.
func anthropicErrType(status int) string {
	switch status {
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 504:
		return "timeout_error"
	}
	if status >= 400 && status < 500 {
		return "invalid_request_error"
	}
	return "api_error"
}

func (n *Node) listModels(w http.ResponseWriter, r *http.Request) {
	nodes := map[string]int{}
	for _, m := range n.modelNames() {
		if n.ownModel(m) {
			nodes[m]++
		}
	}
	for _, a := range n.alive() {
		for _, m := range a.Models {
			nodes[m]++
		}
	}
	names := make([]string, 0, len(nodes))
	for m := range nodes {
		// Announces are checked when they arrive; this keeps the list clean
		// whatever reached the table.
		if protocol.ValidModelName(m) {
			names = append(names, m)
		}
	}
	sort.Strings(names)
	data := []map[string]any{}
	for _, m := range names {
		_, canonical := models.Manifest[m]
		data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "bitllm", "nodes": nodes[m], "canonical": canonical})
	}
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (n *Node) complete(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n.gatewayLive.Add(1)
		defer n.gatewayLive.Add(-1)
		body, err := io.ReadAll(io.LimitReader(r.Body, maxGatewayBody+1))
		if err != nil {
			gatewayErr(w, path, 400, err.Error())
			return
		}
		if len(body) > maxGatewayBody {
			gatewayErr(w, path, 413, msgTooLarge)
			return
		}
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(body, &req) != nil {
			gatewayErr(w, path, 400, "bad json")
			return
		}
		if defuses(path) {
			body = defuseBody(body) // also here, for peers running an older version
			if len(body) > maxGatewayBody {
				gatewayErr(w, path, 413, msgTooLarge)
				return
			}
		}

		// Our own model: private and free, no network hop. While our server
		// is down the request stays here too, unless the operator chose to
		// let peers, who can read it, answer instead. A node that has not yet
		// reached its server cannot tell which names are its own, so until it
		// can, every request stays here.
		if n.cfg.Upstream != "" && !n.upOK.Load() && !n.cfg.PeerFallback {
			if _, ours := n.upstreamModel(req.Model); ours || len(n.modelMap()) == 0 {
				gatewayErr(w, path, 503, fmt.Sprintf("your own model server is down, so this request for %q went nowhere; start it, or run bitllm with --peer-fallback to send such requests to peers (they can read them)", req.Model))
				return
			}
		}
		if n.ownModel(req.Model) {
			n.localTake()
			defer n.localDone()
			resp, stream, err := n.callUpstream(r.Context(), path, body)
			if err != nil {
				pe := err.(*peerError)
				gatewayErr(w, path, pe.status, pe.msg)
				return
			}
			defer resp.Body.Close()
			pipe(w, writeHead(w, stream, ""), resp.Body, stream, &meter{})
			return
		}

		cands := n.candidates(req.Model)
		if len(cands) == 0 && time.Since(n.started) < startupWindow {
			cands = n.awaitCandidates(r.Context(), req.Model)
		}
		if len(cands) == 0 {
			// Gossip may not have reached us yet. Ask the DHT for providers
			// of this model, which is what a node that just started sees.
			n.findModelPeers(r.Context(), req.Model)
			cands = n.candidates(req.Model)
		}
		if len(cands) == 0 {
			gatewayErr(w, path, 503, "no peer serves model "+req.Model+"; see GET /v1/models")
			return
		}
		// Try the best few peers. A peer that has no free slot keeps the
		// request in its own queue for as long as we allow, so this loop only
		// spins when every peer refused at once.
		var last, denied error
		refused := map[peer.ID]bool{} // servers that serve only an allow list we are not on
		saidBusy := map[peer.ID]bool{}
		deadline := time.Now().Add(busyWait)
		for {
			busy, tried := true, 0
			for _, p := range cands {
				if tried == 3 {
					break
				}
				if refused[p] || r.Context().Err() != nil {
					continue
				}
				remaining := time.Until(deadline)
				if remaining <= 0 {
					break
				}
				wait := remaining
				if left := len(cands) - tried; left > 1 {
					wait = min(remaining, peerWait)
				}
				started, err := n.infer(r.Context(), p, path, body, req.Stream, w, wait)
				if err == nil {
					n.peerMem.answered(p, req.Model, time.Now())
					if n.audits != nil {
						n.audits.observe(p, req.Model, body)
					}
					return
				}
				// A local client that hangs up (the user stopped the answer, or
				// the agent closed the request) resets our stream; that is not
				// the peer's failure.
				if r.Context().Err() != nil || errors.Is(err, errLocalGone) {
					return
				}
				if started {
					// Part of an answer went out as a 200. Only dropping the
					// connection tells the client that it did not end.
					log.Printf("peer %s broke mid-response: %s", p, cleanPeerText(err.Error(), maxPeerText))
					n.peerMem.failed(p, req.Model, time.Now())
					panic(http.ErrAbortHandler)
				}
				var pe *peerError
				if errors.As(err, &pe) && notAllowed(pe.status, pe.msg) {
					// Skip it, here and in the retries; another server may answer.
					refused[p], denied = true, err
					n.peerMem.failed(p, req.Model, time.Now())
					continue
				}
				tried++
				if !errors.As(err, &pe) || !isBusy(pe.status, pe.msg) {
					log.Printf("peer %s: %s", p, cleanPeerText(err.Error(), maxPeerText))
					n.peerMem.failed(p, req.Model, time.Now())
					busy = false
					// The time spent on a failure is no reason to skip the next
					// peer: this round is bounded by its three tries.
					if next := time.Now().Add(peerWait); next.After(deadline) {
						deadline = next
					}
				} else {
					saidBusy[p] = true
				}
				last = err
			}
			if tried == 0 || !busy || time.Now().After(deadline) || r.Context().Err() != nil {
				break
			}
			// Everything we asked refused at once. Wait a little, then look
			// at the freshest announces rather than the same stale list.
			time.Sleep(busyBackoff)
			if c := n.candidates(req.Model); len(c) > 0 {
				cands = c
			}
			cands = askedLast(cands, saidBusy)
		}
		if last == nil {
			last = denied // every server for this model serves only its own allow list
		}
		if last == nil {
			last = errors.New("no peer answered before the wait ran out")
		}
		var pe *peerError
		if errors.As(last, &pe) {
			gatewayErr(w, path, peerStatus(pe.status), "all peers refused: "+cleanPeerText(pe.msg, maxPeerText))
		} else {
			gatewayErr(w, path, 502, "all peers failed: "+cleanPeerText(last.Error(), maxPeerText))
		}
	}
}

// askedLast moves the peers that refused this request behind the ones not asked
// yet, keeping each group's order. Each round tries only a few peers, so without
// this the same few could refuse every round and hide the rest.
func askedLast(cands []peer.ID, asked map[peer.ID]bool) []peer.ID {
	out := make([]peer.ID, 0, len(cands))
	for _, p := range cands {
		if !asked[p] {
			out = append(out, p)
		}
	}
	for _, p := range cands {
		if asked[p] {
			out = append(out, p)
		}
	}
	return out
}

// awaitCandidates looks for candidates until one shows up, startupWait runs
// out or the request goes away.
func (n *Node) awaitCandidates(ctx context.Context, model string) []peer.ID {
	limit := time.NewTimer(startupWait)
	defer limit.Stop()
	poll := time.NewTicker(startupPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-limit.C:
			return nil
		case <-poll.C:
			if c := n.candidates(model); len(c) > 0 {
				return c
			}
		}
	}
}

func (n *Node) status(w http.ResponseWriter, r *http.Request) {
	serving := n.modelNames()
	conns := []map[string]any{}
	for _, c := range n.host.Network().Conns() {
		conns = append(conns, map[string]any{
			"peer":    c.RemotePeer().String(),
			"addr":    c.RemoteMultiaddr().String(),
			"dir":     c.Stat().Direction.String(),
			"relayed": strings.Contains(c.RemoteMultiaddr().String(), "/p2p-circuit"),
		})
	}
	out := map[string]any{
		"version":     Version,
		"id":          n.host.ID().String(),
		"addrs":       n.Addrs(),
		"conns":       conns,
		"serving":     serving,
		"upstream_up": n.cfg.Upstream != "" && n.upOK.Load(),
		"inflight":    n.inflight.Load(),
		"queued":      n.queueDepth(),
		"refusals":    n.refused.snapshot(),
		"uptime":      int64(time.Since(n.started).Seconds()),
		"ledger":      n.ledger.Snapshot(),
	}
	if n.cfg.UpdateStatus != nil {
		out["update"] = n.cfg.UpdateStatus()
	}
	if n.limits != nil {
		out["limits"] = n.limits
	}
	if e := n.announceErr.Load(); e != nil && *e != "" {
		out["announce_error"] = *e
	}
	json.NewEncoder(w).Encode(out)
}

func (n *Node) account(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"node": n.host.ID().String(), "claim": nil}
	if c, ok := n.claims.Get(n.host.ID().String()); ok {
		out["claim"] = c
		out["owner"] = account.ChecksumAddress(c.Owner)
	}
	json.NewEncoder(w).Encode(out)
}

// accountMessage returns the text a wallet must sign to claim this node.
func (n *Node) accountMessage(w http.ResponseWriter, r *http.Request) {
	var req struct{ Owner, Name string }
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	c, err := account.NewClaim(n.host.ID(), req.Owner, req.Name)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"message": c.Message(), "time": c.Time})
}

func (n *Node) accountLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner, Name string
		Time        int64
		OwnerSig    string `json:"owner_sig"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	if err := account.SignatureShapeError(req.OwnerSig); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	c, err := n.Link(req.Owner, req.Name, req.Time, req.OwnerSig)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"claim": c, "owner": account.ChecksumAddress(c.Owner)})
}
