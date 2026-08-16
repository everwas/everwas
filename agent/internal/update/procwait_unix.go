//go:build !windows

package update

import "syscall"

// ProcessExited reports whether the process is gone. Signal 0 is the standard
// existence probe: it delivers nothing and only reports whether the target is
// still there and reachable.
func ProcessExited(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return false
	}
	// EPERM means the process exists but belongs to someone else. That still
	// counts as running.
	return err != syscall.EPERM
}
