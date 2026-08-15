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
func SpawnFinalizer(staged, target string) error {
	if staged == "" || target == "" {
		return fmt.Errorf("%w: empty staged or target path", ErrSwap)
	}
	cmd := exec.Command(staged, //nolint:gosec // staged is signature verified before we get here
		"update-finalize",
		"--pid", strconv.Itoa(os.Getpid()),
		"--target", target,
		"--staged", staged,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: spawn finalizer: %v", ErrSwap, err)
	}
	// Do not Wait: the finalizer outlives us by design.
	return cmd.Process.Release()
}
