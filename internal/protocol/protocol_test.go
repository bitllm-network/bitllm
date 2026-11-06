package protocol

import (
	"strings"
	"testing"

	"github.com/bitllm-org/bitllm/internal/models"
)

func TestValidModelName(t *testing.T) {
	for _, s := range []string{
		"Qwen/Qwen3.8-Flash-Next", "my-lab/new-model", "llama3:8b", "phi@v2", "a+b=c", "m",
		strings.Repeat("x", MaxModelName),
	} {
		if !ValidModelName(s) {
			t.Fatalf("rejected %q", s)
		}
	}
	for name := range models.Manifest {
		if !ValidModelName(name) {
			t.Fatalf("rejected the canonical name %q", name)
		}
	}
	for _, s := range []string{
		"", "-m", "a b", "a;b", "a'b", "a\"b", "a`b", "$m", "$(m)", "a\nb", "a\tb", "a|b", "a&b", "a>b", "a\\b", "a%b", "模型",
		strings.Repeat("x", MaxModelName+1),
	} {
		if ValidModelName(s) {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestValidAnnounceChecksEveryName(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ok := Announce{Models: []string{"org/m:1"}, Audit: []string{"org/m:1"}, Weights: map[string]string{"org/m:1": hash}, Max: 1}
	if !ValidAnnounce(&ok) {
		t.Fatal("rejected a normal announce")
	}
	for name, a := range map[string]Announce{
		"model":   {Models: []string{"m", "a b"}, Max: 1},
		"audit":   {Models: []string{"m"}, Audit: []string{"a b"}, Max: 1},
		"weights": {Models: []string{"m"}, Weights: map[string]string{"a b": hash}, Max: 1},
	} {
		if ValidAnnounce(&a) {
			t.Fatalf("accepted a bad name in %s", name)
		}
	}
}

func TestValidAnnounce(t *testing.T) {
	ok := Announce{Models: []string{"m"}, Free: 1, Max: 2, Q: 3}
	if !ValidAnnounce(&ok) {
		t.Fatal("rejected a normal announce")
	}
	many := make([]string, MaxAnnounceModels+1)
	for i := range many {
		many[i] = "m"
	}
	for name, a := range map[string]Announce{
		"no models":  {Max: 1},
		"too many":   {Models: many, Max: 1},
		"long name":  {Models: []string{strings.Repeat("x", MaxModelName+1)}, Max: 1},
		"empty name": {Models: []string{""}, Max: 1},
		"free > max": {Models: []string{"m"}, Free: 3, Max: 2},
		"negative":   {Models: []string{"m"}, Free: -1, Max: 2},
		"huge max":   {Models: []string{"m"}, Max: MaxAnnounceSlots + 1},
		"zero slots": {Models: []string{"m"}},
		"negative q": {Models: []string{"m"}, Max: 1, Q: -1},
		"huge q":     {Models: []string{"m"}, Max: 1, Q: MaxQueueDepth*64 + 1},
	} {
		if ValidAnnounce(&a) {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestAnnounceLocLimit(t *testing.T) {
	a := Announce{Models: []string{"m"}, Free: 1, Max: 1, Loc: strings.Repeat("u", MaxLocJSON)}
	if !ValidAnnounce(&a) {
		t.Fatal("a loc of MaxLocJSON bytes must pass")
	}
	a.Loc += "u"
	if ValidAnnounce(&a) {
		t.Fatal("a longer loc must be refused")
	}
}
