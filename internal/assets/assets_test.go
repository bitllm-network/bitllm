package assets

import (
	"bytes"
	"testing"
)

// The page must carry its own fonts: if the placeholder stops matching, the
// dashboard silently falls back to whatever each OS has.
func TestUIInlinesFonts(t *testing.T) {
	if bytes.Contains(UI, []byte("/*FONTS*/")) {
		t.Error("the fonts placeholder was not replaced")
	}
	if got := bytes.Count(UI, []byte("@font-face")); got != 5 {
		t.Errorf("got %d @font-face rules, want 5", got)
	}
	for _, family := range []string{`font-family: "JetBrains Mono"`, `font-family: "Arimo"`} {
		if !bytes.Contains(UI, []byte(family)) {
			t.Errorf("no %s rule in the page", family)
		}
	}
}
