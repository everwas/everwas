//go:build !windows

package patch

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setProcAttr puts the package manager in its own process group. Without it
// a cancelled run signals the direct child only: apt-get dies, the dpkg it
// spawned keeps /var/lib/dpkg/lock-frontend, and the retry burns the whole
// lock budget waiting for a process the agent itself orphaned. Worse, the
// orphan inherits the stdout pipe, so the drain never finishes and runCmd
// blocks past its own timeout while holding the install gate.
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateGroup signals the whole process group: SIGTERM first, so a
// package manager gets the chance to unwind, then SIGKILL after killGrace
// for anything that ignored it. The negative pid is the group, which works
// because setProcAttr made the child a group leader.
//
// Returning nil tells os/exec to report the context's own error, which is
// what the caller wants to see: "timeout", not "signal: terminated".
func terminateGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		// The group is already gone, or we never became a leader. Fall back
		// to the child alone rather than signalling nothing.
		return cmd.Process.Kill()
	}
	time.AfterFunc(killGrace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	return nil
}
