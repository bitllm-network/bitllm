// Package assets holds files the binary carries: the dashboard page and the
// built-in canary prompts.
package assets

import _ "embed"

// UI is the dashboard served at the gateway root.
//
//go:embed ui.html
var UI []byte

// Canaries is the canary prompt pool, one prompt per line.
//
//go:embed canaries.txt
var Canaries string
