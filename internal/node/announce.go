package node

import (
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/bitllm-org/bitllm/internal/models"
	"github.com/bitllm-org/bitllm/internal/protocol"
)

// announcedModels picks what fits in an announce: valid names only, canonical
// ones first and then sorted, up to MaxAnnounceModels. One bad name or one name
// too many would make every node refuse the whole announce. Only the announce
// is capped: a local client can still ask its own gateway for any model.
func announcedModels(names []string) (kept []string, omitted int) {
	var canonical, other []string
	for _, m := range names {
		if !protocol.ValidModelName(m) {
			omitted++
		} else if _, ok := models.Manifest[m]; ok {
			canonical = append(canonical, m)
		} else {
			other = append(other, m)
		}
	}
	slices.Sort(canonical)
	slices.Sort(other)
	kept = append(canonical, other...)
	if len(kept) > protocol.MaxAnnounceModels {
		omitted += len(kept) - protocol.MaxAnnounceModels
		kept = kept[:protocol.MaxAnnounceModels]
	}
	return kept, omitted
}

// announcedSlots fits --concurrency into the range an announce may state. The
// serving queue keeps using the configured number.
func announcedSlots(concurrency, inflight int) (free, slots int, clamped bool) {
	slots = min(max(concurrency, 1), protocol.MaxAnnounceSlots)
	free = min(max(concurrency-inflight, 0), slots)
	return free, slots, slots != concurrency
}

// onlyAnnounced keeps the entries of names that are in kept.
func onlyAnnounced(names, kept []string) []string {
	var out []string
	for _, m := range names {
		if slices.Contains(kept, m) {
			out = append(out, m)
		}
	}
	return out
}

// announcedWeights keeps the hashes of announced models, since a hash for any
// other model makes the announce invalid.
func announcedWeights(weights map[string]string, kept []string) map[string]string {
	var out map[string]string
	for m, h := range weights {
		if slices.Contains(kept, m) && models.ValidHash(h) {
			if out == nil {
				out = map[string]string{}
			}
			out[m] = h
		}
	}
	return out
}

// announced returns the models this node gossips right now: none while its
// server is down.
func (n *Node) announced() []string {
	if a := n.lastAnn.Load(); a != nil {
		return a.Models
	}
	return nil
}

// announceProblems is what kept the last announce from saying everything.
type announceProblems struct {
	omitted int    // model names left out
	clamped bool   // --concurrency does not fit an announce
	publish string // error from the last publish
}

// String is the announce_error value in GET /status; empty when all is well.
func (p announceProblems) String() string {
	var parts []string
	if p.omitted > 0 {
		parts = append(parts, fmt.Sprintf("%d models not announced", p.omitted))
	}
	if p.clamped {
		parts = append(parts, fmt.Sprintf("--concurrency announced within 1 to %d", protocol.MaxAnnounceSlots))
	}
	if p.publish != "" {
		parts = append(parts, "publish failed: "+p.publish)
	}
	return strings.Join(parts, "; ")
}

// announceLog logs each problem once, not once per announce period.
type announceLog struct {
	omitted   int
	clamped   bool
	published map[string]bool
}

// maxPublishErrors bounds the publish errors remembered as already logged.
const maxPublishErrors = 16

func (l *announceLog) report(p announceProblems, slots int) {
	if p.omitted != l.omitted {
		log.Printf("announce: %d models left out (invalid names, or more than %d); canonical names go first", p.omitted, protocol.MaxAnnounceModels)
		l.omitted = p.omitted
	}
	if p.clamped && !l.clamped {
		log.Printf("announce: --concurrency is outside 1 to %d, announcing %d slots", protocol.MaxAnnounceSlots, slots)
	}
	l.clamped = p.clamped
	if p.publish != "" && !l.published[p.publish] && len(l.published) < maxPublishErrors {
		if l.published == nil {
			l.published = map[string]bool{}
		}
		l.published[p.publish] = true
		log.Printf("announce: publish failed: %s", p.publish)
	}
}
