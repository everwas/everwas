package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Swap failures.
var (
	ErrSwap          = errors.New("update: binary swap failed")
	ErrNoBackup      = errors.New("update: no backup binary to restore")
	ErrNoFinalizer   = errors.New("update: external finalizer is not used on this platform")
	ErrSameFilenames = errors.New("update: staged artifact and target are the same file")
)

// SwapResult describes what a successful swap left on disk.
type SwapResult struct {
	Target string // the path now holding the new binary
	Backup string // the previous binary, kept until the new one reports healthy
}

// Swap replaces target with staged, keeping the previous binary at
// BackupPath(target). The caller is expected to exit(0) afterwards so the
// service manager starts the new binary; nothing in this package restarts the
// process for you.
//
// The sequence is copy, rename-away, rename-in, so target is never missing
// for longer than one rename and a mid-sequence failure is undone.
func Swap(target, staged string) (SwapResult, error) {
	if target == "" || staged == "" {
		return SwapResult{}, fmt.Errorf("%w: empty target or staged path", ErrSwap)
	}
	if sameFile(target, staged) {
		return SwapResult{}, ErrSameFilenames
	}
	backup := BackupPath(target)

	tmp := target + ".new"
	if err := copyFile(staged, tmp, 0o755); err != nil {
		return SwapResult{}, fmt.Errorf("%w: stage next to target: %v", ErrSwap, err)
	}
	// A leftover backup from a previous update would block the rename on
	// Windows and confuse rollback everywhere else.
	_ = os.Remove(backup)
	if err := os.Rename(target, backup); err != nil {
		_ = os.Remove(tmp)
		return SwapResult{}, fmt.Errorf("%w: move current binary aside: %v", ErrSwap, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		// Put the old binary back before giving up: an agent that is running
		// an old version beats a host with no agent binary at all.
		_ = os.Rename(backup, target)
		_ = os.Remove(tmp)
		return SwapResult{}, fmt.Errorf("%w: move new binary into place: %v", ErrSwap, err)
	}
	return SwapResult{Target: target, Backup: backup}, nil
}

// RestoreBackup moves the saved binary back over target. It is the rollback
// half of Swap and runs before the agent starts, never from inside the
// process being replaced.
func RestoreBackup(target string) error {
	backup := BackupPath(target)
	fi, err := os.Stat(backup)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrNoBackup, backup)
	}
	// Rename over an existing file is fine here because the process holding
	// the broken binary has already exited.
	if err := os.Rename(backup, target); err != nil {
		return fmt.Errorf("%w: restore backup: %v", ErrSwap, err)
	}
	return os.Chmod(target, 0o755)
}

// RemoveBackup deletes the saved previous binary.
//
// Nothing in the update path calls this. Probation ending does NOT retire the
// backup: one spare generation costs about twenty megabytes and is the only
// recovery an operator has for a defect that shows up after the probation
// window. The next Swap overwrites it, so exactly one generation is kept.
func RemoveBackup(target string) error {
	err := os.Remove(BackupPath(target))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// HasBackup reports whether a previous binary is still on disk.
func HasBackup(target string) bool {
	fi, err := os.Stat(BackupPath(target))
	return err == nil && fi.Mode().IsRegular()
}

// ExecutablePath resolves the running binary through any symlinks so an
// update replaces the real file rather than a link in /usr/local/bin.
func ExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Chmod(dst, mode)
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
