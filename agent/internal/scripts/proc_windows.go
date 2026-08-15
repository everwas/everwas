//go:build windows

package scripts

import (
	"errors"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGuard wraps the child in a Job Object so a timeout kills the whole
// tree. KILL_ON_JOB_CLOSE also cleans up if the agent itself dies.
type procGuard struct {
	job windows.Handle
}

func newProcGuard() *procGuard { return &procGuard{} }

func (g *procGuard) beforeStart(*exec.Cmd) {}

// afterStart assigns the started child to a fresh job object. Failure is
// tolerated: kill() then falls back to terminating just the child.
func (g *procGuard) afterStart(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return
	}
	g.job = job
}

func (g *procGuard) kill(cmd *exec.Cmd) error {
	if g.job != 0 {
		if err := windows.TerminateJobObject(g.job, 1); err == nil {
			return nil
		}
	}
	if cmd == nil || cmd.Process == nil {
		return errors.New("no process")
	}
	return cmd.Process.Kill()
}

func (g *procGuard) release() {
	if g.job != 0 {
		windows.CloseHandle(g.job)
		g.job = 0
	}
}
