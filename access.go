package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bitllm-network/bitllm/internal/account"
	"github.com/bitllm-network/bitllm/internal/members"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

// memberSet returns the current member set, recomputed at most every 10 s.
func (n *Node) memberSet() *members.MemberSet {
	n.accessMu.Lock()
	defer n.accessMu.Unlock()
	if n.access != nil && time.Since(n.accessAt) < 10*time.Second {
		return n.access
	}
	n.access, n.accessAt = n.members.Compute(), time.Now()
	return n.access
}

// invalidateMembers drops the cached member set after a change.
func (n *Node) invalidateMembers() {
	n.accessMu.Lock()
	n.access = nil
	n.accessMu.Unlock()
	n.servedMu.Lock()
	n.servedEver = nil
	n.servedMu.Unlock()
}

// servedSets are the accounts owning a node that has a receipt as server:
// ever (10 invites) and within the contributor window (no quota).
func (n *Node) servedSets() (ever, recent map[string]bool) {
	n.servedMu.Lock()
	defer n.servedMu.Unlock()
	if n.servedEver != nil && time.Since(n.servedAt) < 10*time.Second {
		return n.servedEver, n.servedRecent
	}
	since := time.Now().Add(-members.ContributorWindow).Unix()
	servers := n.receipts.ServersSince(since) // node -> served recently
	ever, recent = map[string]bool{}, map[string]bool{}
	for s, rec := range servers {
		if c, ok := n.claims.Get(s); ok {
			ever[c.Owner] = true
			recent[c.Owner] = recent[c.Owner] || rec
		}
	}
	n.servedEver, n.servedRecent, n.servedAt = ever, recent, time.Now()
	return ever, recent
}

// accountOf is the account that owns node p, or "".
func (n *Node) accountOf(p peer.ID) string {
	if c, ok := n.claims.Get(p.String()); ok {
		return c.Owner
	}
	return ""
}

// The three access rules, as the server applies them to a request from remote.
const (
	msgBusy       = "busy"
	msgQueueFull  = "busy: the queue for this server is full right now. Another server may still answer; retry in a moment"
	msgNotAllowed = "not allowed: this server serves only the peers its owner listed with --allow"
	msgUninvited  = "busy: this server serves uninvited nodes only when it is idle. Ask a member for an invite (bitllm invite), then link this node to your account"
	msgQuotaSpent = "daily free quota on this server used up (its owner set one). Another server may still answer; sharing a model removes the limit, or try again after 00:00 UTC"
)

// ticket is a taken serving slot and what the job will be charged to.
type ticket struct {
	peer     peer.ID
	key      string    // daily quota key, "" when not counted (contributors, no limit)
	limit    int64     // daily limit for key, 0 = none
	reserved int64     // credits held against limit until the job ends
	open     bool      // an uninvited node served because the server is --open
	start    time.Time // when the job took its slot, for the service-time average

	// An open job is also charged to the shared pool every uninvited node
	// draws on, so the pool bounds the server however many ids ask.
	poolLimit    int64
	poolReserved int64
}

const openPool = "open-pool" // quota key shared by all uninvited nodes on an open server

// admission is a taken slot, or a place in the queue.
type admission struct {
	t ticket
	w *waiter // set when the request is queued
}

// openSlots is how many slots uninvited nodes may hold at once on an open
// server, so members and contributors always find room.
func (n *Node) openSlots() int32 {
	if n.cfg.OpenSlots > 0 {
		return int32(min(n.cfg.OpenSlots, n.cfg.Concurrency))
	}
	return int32(max(1, n.cfg.Concurrency/2))
}

// refusals counts why this server said no, for GET /status and GET /queue.
// It is a small diagnostic, not policy: nothing reads it to decide anything.
type refusals struct {
	busy, queue, quota, allow atomic.Int64
}

func (r *refusals) snapshot() map[string]int64 {
	return map[string]int64{
		"busy":  r.busy.Load(),
		"queue": r.queue.Load(),
		"quota": r.quota.Load(),
		"allow": r.allow.Load(),
	}
}

// admit takes a serving slot for remote, or queues it, or says why not. The
// caller must call done when the job ends, and must watch a.w.ready when a
// waiter is returned.
//
// amount is the worst case the job may cost in credits, reserved against the
// daily quota until it ends.
func (n *Node) admit(remote peer.ID, model string, now time.Time, wait time.Duration, amount int64) (status int, msg string, a admission) {
	if len(n.cfg.Allow) > 0 && !n.cfg.Allow[remote] {
		n.refused.allow.Add(1)
		return 403, fmt.Sprintf("%s. Your node is %s; send that id to the owner to be added", msgNotAllowed, remote), a
	}
	account := n.accountOf(remote)
	member, contributor := true, false
	if n.members.Enabled() {
		ms := n.memberSet()
		member = account != "" && ms.Member[account]
		contributor = member && ms.Contributors[account]
	}
	if len(n.cfg.Allow) > 0 {
		member = true // the owner's list replaces the invite rule on this server
	}
	// An open server serves uninvited nodes on spare slots, while the shared
	// pool for all of them lasts. Peer ids are free, so the per-node quota
	// alone would not bound it.
	open := !member && n.cfg.Open && n.quota.Room(openPool, n.cfg.OpenDaily, now) > 0
	class := classMember
	switch {
	case contributor:
		class = classContributor
	case member:
		class = classMember
	case open:
		class = classOpen
	default:
		class = classIdle
	}
	// What the job is charged to. A contributor is never charged; an
	// uninvited node on an open server shares one pool; anyone else pays
	// against their account, or its node id when it has none.
	t := ticket{peer: remote, open: open, limit: n.cfg.FreeDaily, poolLimit: n.cfg.OpenDaily, start: now}
	switch {
	case class == classContributor:
	case class == classOpen:
		// Charged per node as well as to the shared pool, so one id cannot
		// use the pool more than its own daily share.
		t.key = "node:" + remote.String()
	default:
		t.key = account
		if t.key == "" || !member {
			t.key = "node:" + remote.String()
		}
	}
	// The job reserves its worst case now, whether it runs at once or waits,
	// so parallel jobs cannot both pass the same remaining quota, and a
	// queued job cannot start on quota another job already took.
	reserved, ok := n.quota.Take(t.key, amount, t.limit, now)
	if !ok {
		n.refused.quota.Add(1)
		return 429, msgQuotaSpent, a
	}
	t.reserved = reserved
	if open {
		t.poolReserved, _ = n.quota.Take(openPool, amount, t.poolLimit, now)
	}
	n.qmu.Lock()
	// Grant whoever already waits and fits, so a new request cannot take
	// the slot a queued one was about to get. After this no waiter fits.
	n.dispatchLocked()
	if n.tryTakeLocked(remote, class) {
		n.qmu.Unlock()
		return 0, "", admission{t: t}
	}
	if wait <= 0 {
		spent := open && n.openInflight.Load() >= n.openSlots()
		n.qmu.Unlock()
		n.quota.Drop(t.key, t.reserved, now)
		if open {
			n.quota.Drop(openPool, t.poolReserved, now)
		}
		n.refused.busy.Add(1)
		if class == classIdle || spent {
			return 429, msgUninvited, a
		}
		return 429, msgBusy, a
	}
	w := &waiter{peer: remote, model: model, class: class, at: now,
		key: t.key, limit: t.limit, amount: amount, open: open, pool: t.poolLimit,
		ready: make(chan struct{})}
	w.t = t
	if !n.enqueueLocked(w) {
		n.qmu.Unlock()
		n.quota.Drop(t.key, t.reserved, now)
		if open {
			n.quota.Drop(openPool, t.poolReserved, now)
		}
		n.refused.queue.Add(1)
		return 429, msgQueueFull, a
	}
	n.dispatchLocked()
	n.qmu.Unlock()
	return 202, "", admission{w: w}
}

// done releases a ticket's slot, settles its quota reservation and charges
// the credits the job used.
func (n *Node) done(t ticket, credits int64, now time.Time) {
	n.qmu.Lock()
	n.releaseLocked(t)
	// Dispatch before unlocking: otherwise the slot is free but unclaimed
	// for a moment, and a new request could take it ahead of a waiter.
	n.dispatchLocked()
	n.qmu.Unlock()
	if credits > 0 { // a job that served nothing says nothing about the wait
		if d := now.Sub(t.start); d > 0 {
			if cur := n.svc.Load(); cur > 0 {
				n.svc.Store((cur*3 + d.Nanoseconds()) / 4)
			} else {
				n.svc.Store(d.Nanoseconds())
			}
		}
	}
	n.quota.Settle(t.key, t.reserved, credits, now)
	if t.open {
		n.quota.Settle(openPool, t.poolReserved, credits, now)
	}
}

// notAllowed reports whether a peer refused us for good: its owner serves
// only an allow list we are not on. Asking it again will not help.
func notAllowed(status int, msg string) bool {
	return status == 403 && strings.HasPrefix(msg, msgNotAllowed)
}

// isBusy reports whether a refusal is temporary, worth waiting out.
func isBusy(status int, msg string) bool {
	return status == 429 && strings.HasPrefix(msg, msgBusy)
}

func (n *Node) readMembers(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		var it members.Item
		if json.Unmarshal(msg.Data, &it) == nil && n.members.Add(it) {
			n.invalidateMembers()
		}
	}
}

// publishMember stores an invite or revocation and gossips it.
func (n *Node) publishMember(it members.Item) bool {
	if !n.members.Add(it) {
		return false
	}
	n.invalidateMembers()
	b, _ := json.Marshal(it)
	n.memTop.Publish(context.Background(), b)
	// Gossip sent before the mesh has formed is lost; say it once more.
	time.AfterFunc(5*time.Second, func() { n.memTop.Publish(context.Background(), b) })
	return true
}

// MemberInfo is what the dashboard shows about an account.
type MemberInfo struct {
	Enabled      bool   `json:"enabled"`
	Account      string `json:"account,omitempty"`
	Member       bool   `json:"member"`
	Root         bool   `json:"root"`
	Contributor  bool   `json:"contributor"`
	InvitedBy    string `json:"invited_by,omitempty"`
	InvitesUsed  int    `json:"invites_used"`
	InvitesLimit int    `json:"invites_limit"` // -1: unlimited
	Members      int    `json:"members"`
}

func (n *Node) memberInfo(acct string) MemberInfo {
	ms := n.memberSet()
	info := MemberInfo{Enabled: n.members.Enabled(), Account: acct, Members: len(ms.Member)}
	if acct == "" {
		return info
	}
	info.Member = ms.Member[acct]
	info.Root = ms.IsRoot(acct)
	info.Contributor = ms.Contributors[acct]
	if inv := ms.InvitedBy[acct]; inv != "" {
		info.InvitedBy = account.ChecksumAddress(inv)
	}
	info.InvitesUsed = ms.Used[acct]
	info.InvitesLimit = ms.InviteLimit(acct)
	return info
}

func (n *Node) membersMe(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(n.memberInfo(n.accountOf(n.host.ID())))
}

// membersPost accepts a signed invite or revocation (from the CLI) and gossips it.
func (n *Node) membersPost(w http.ResponseWriter, r *http.Request) {
	var it members.Item
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&it); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if err := account.SignatureShapeError(sigOf(it)); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if err := n.members.Check(it); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if !n.publishMember(it) {
		jsonErr(w, 400, "already known")
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func sigOf(it members.Item) string {
	switch {
	case it.Invite != nil:
		return it.Invite.Sig
	case it.Revoke != nil:
		return it.Revoke.Sig
	}
	return ""
}

// membersList shows every member with who invited it and how much its nodes
// served lately, so a root can spot a receipt farm and revoke it.
func (n *Node) membersList(w http.ResponseWriter, r *http.Request) {
	ms := n.memberSet()
	since := time.Now().Add(-members.ContributorWindow).Unix()
	jobs := n.receipts.JobsSince(since)
	perAccount := map[string]int{}
	for node, j := range jobs {
		if c, ok := n.claims.Get(node); ok {
			perAccount[c.Owner] += j
		}
	}
	type row struct {
		Account     string `json:"account"`
		InvitedBy   string `json:"invited_by,omitempty"`
		Root        bool   `json:"root,omitempty"`
		Sharer      bool   `json:"sharer"`
		Contributor bool   `json:"contributor"`
		Invited     int    `json:"invited"`
		Jobs7d      int    `json:"jobs_7d"`
	}
	out := []row{}
	for a := range ms.Member {
		rw := row{Account: account.ChecksumAddress(a), Root: ms.IsRoot(a), Sharer: ms.Sharers[a],
			Contributor: ms.Contributors[a], Invited: ms.Used[a], Jobs7d: perAccount[a]}
		if inv := ms.InvitedBy[a]; inv != "" {
			rw.InvitedBy = account.ChecksumAddress(inv)
		}
		out = append(out, rw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Jobs7d > out[j].Jobs7d })
	json.NewEncoder(w).Encode(out)
}
