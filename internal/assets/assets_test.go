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
	if bytes.Contains(UI, []byte(`base64,"`)) {
		t.Error("a font rule has no data")
	}
}

// The OFL requires every copy of the fonts to carry their notices and license.
func TestUICarriesFontLicenses(t *testing.T) {
	for _, want := range []string{
		"The JetBrains Mono Project Authors",
		"The Arimo Project Authors",
		"SIL OPEN FONT LICENSE Version 1.1",
	} {
		if !bytes.Contains(UI, []byte(want)) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}
