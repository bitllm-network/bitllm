package node

import (
	"fmt"
	"testing"

	"github.com/bitllm-org/bitllm/internal/protocol"
)

// A server listing too many models must stay visible, not vanish: peers reject
// the whole announce otherwise.
func TestCapAnnounceStaysValid(t *testing.T) {
	var models []string
	for i := protocol.MaxAnnounceModels + 4; i > 0; i-- {
		models = append(models, fmt.Sprintf("model-%02d", i))
	}
	first, last := "model-01", models[0]
	weights := map[string]string{
		first: fmt.Sprintf("%064d", 1),
		last:  fmt.Sprintf("%064d", 2),
	}
	a := protocol.Announce{Models: models, Free: 1, Max: 1, Audit: []string{first, last}, Weights: weights}
	if protocol.ValidAnnounce(&a) {
		t.Fatal("the uncapped announce should be invalid, or this test proves nothing")
	}

	capAnnounce(&a)
	if !protocol.ValidAnnounce(&a) {
		t.Fatalf("capped announce is invalid: %+v", a)
	}
	if len(a.Models) != protocol.MaxAnnounceModels || a.Models[0] != first {
		t.Errorf("kept %d models starting %q", len(a.Models), a.Models[0])
	}
	if len(a.Audit) != 1 || a.Audit[0] != first {
		t.Errorf("audit: %v", a.Audit)
	}
	if _, ok := a.Weights[last]; ok || len(a.Weights) != 1 {
		t.Errorf("weights: %v", a.Weights)
	}
	if len(weights) != 2 {
		t.Error("the operator's weights map was changed")
	}
}
