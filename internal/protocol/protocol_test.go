package protocol

import (
	"strings"
	"testing"
)

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
