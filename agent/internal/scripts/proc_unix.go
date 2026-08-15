//go:build !windows

package scripts

import (
	"errors"
	"os/exec"
	"syscall"
)

// procGuard puts the child in its own process group so a timeout kills the
// whole tree — a script that backgrounds `sleep 3600` must not outlive it.
type procGuard struct{}

func newProcGuard() *procGuard { return &procGuard{} }

func (g *procGuard) beforeStart(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (g *procGuard) afterStart(*exec.Cmd) {}

// kill signals the whole process group. The negative pid is the group, which
// works because beforeStart made the child a group leader.
func (g *procGuard) kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("no process")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill() // group gone; try the leader alone
	}
	return nil
}

func (g *procGuard) release() {}
