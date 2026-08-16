//go:build windows

package patch

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setProcAttr gives the child its own console process group. Windows patching
// goes through the Windows Update Agent COM API in this process, so runCmd
// here only ever drives small query helpers; the group exists so a cancelled
// one cannot take the agent's own console with it.
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// terminateGroup kills the child. There is no unix style group signal here,
// and the process tree teardown that scripts.procGuard does with a Job Object
// is not worth carrying for the query helpers this package runs on Windows.
func terminateGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
