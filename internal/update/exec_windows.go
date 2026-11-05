//go:build windows

package update

import "errors"

// InPlace is false on Windows: a running .exe can be renamed but not
// overwritten, so the new binary is installed and used at the next start.
const InPlace = false

// ErrNoExec is returned by Restart on Windows.
var ErrNoExec = errors.New("windows cannot restart in place")

// Restart cannot work on Windows.
func Restart(path string, argv, env []string) error { return ErrNoExec }
