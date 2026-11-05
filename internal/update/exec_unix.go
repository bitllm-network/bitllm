//go:build !windows

package update

import "syscall"

// InPlace reports that this platform can restart into a new binary without
// losing the pid, the terminal or the systemd unit.
const InPlace = true

// Restart replaces this process with the binary at path, keeping argv and
// environment. It does not return on success.
func Restart(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
