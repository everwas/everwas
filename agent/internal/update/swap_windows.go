//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// BackupPath keeps the ".exe" extension on the backup. A running Windows
// executable cannot be overwritten but it can be renamed, which is what makes
// the in-place swap work at all; keeping the extension means the file stays
// launchable if we have to roll back by starting it directly.
func BackupPath(target string) string {
	base := target
	if strings.EqualFold(".exe", filepath.Ext(target)) {
		base = target[:len(target)-len(".exe")]
	}
	return base + ".old.exe"
}

// NeedsFinalizer reports whether SpawnFinalizer is a usable fallback here.
func NeedsFinalizer() bool { return true }

// SpawnFinalizer is the fallback for the case where the in-place rename swap
// is refused, typically because a scanner or another process holds a handle
// on the running image. It launches the staged binary with a hidden
// "update-finalize" subcommand that waits for this process to exit, performs
// the swap from outside, and starts the service again.
//
// stateDir and version are passed through so the finalizer can write its own
// outcome where the agent will find it. Without that, a finalizer that times
// out waiting for the parent exits into the void and the host stays on the
// old version with nobody the wiser.
func SpawnFinalizer(staged, target, stateDir, version string) (int, error) {
	if staged == "" || target == "" {
		return 0, fmt.Errorf("%w: empty staged or target path", ErrSwap)
	}
	args := []string{
		"update-finalize",
		"--pid", strconv.Itoa(os.Getpid()),
		"--target", target,
		"--staged", staged,
	}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	if version != "" {
		args = append(args, "--version", version)
	}
	cmd := exec.Command(staged, args...) //nolint:gosec // staged is signature verified before we get here
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS,
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("%w: spawn finalizer: %v", ErrSwap, err)
	}
	pid := cmd.Process.Pid
	// Do not Wait: the finalizer outlives us by design.
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("%w: release finalizer: %v", ErrSwap, err)
	}
	return pid, nil
}
