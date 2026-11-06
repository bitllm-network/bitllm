// Package assets holds files the binary carries: the dashboard page and the
// built-in canary prompts.
package assets

import (
	"bytes"
	_ "embed"
)

//go:embed ui.html
var page []byte

// fontsCSS holds the dashboard's fonts as data URLs, written by
// scripts/fonts-css.sh. It stays out of ui.html so the page remains readable.
//
//go:embed fonts.css
var fontsCSS []byte

// UI is the dashboard served at the gateway root, with its fonts inlined: the
// page works offline and makes no other request for them.
var UI = bytes.Replace(page, []byte("/*FONTS*/"), fontsCSS, 1)

// Canaries is the canary prompt pool, one prompt per line.
//
//go:embed canaries.txt
var Canaries string
