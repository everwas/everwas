package update

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBackupPathKeepsWindowsExtension(t *testing.T) {
	got := BackupPath(filepath.Join("dir", "openrmm-agent"))
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(got, ".old.exe") {
			t.Errorf("BackupPath = %s, want a .old.exe suffix", got)
		}
		return
	}
	if !strings.HasSuffix(got, ".old") {
		t.Errorf("BackupPath = %s, want a .old suffix", got)
	}
}

func TestSwapReplacesAndKeepsBackup(t *testing.T) {
	binDir := t.TempDir()
	stageDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "old build")
	staged := fakeBinary(t, stageDir, "openrmm-agent-2.0.0", "new build")

	res, err := Swap(target, staged)
	if err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := readFile(t, target); got != "new build" {
		t.Errorf("target = %q, want the new build", got)
	}
	if got := readFile(t, res.Backup); got != "old build" {
		t.Errorf("backup = %q, want the old build", got)
	}
	if !HasBackup(target) {
		t.Error("HasBackup = false after a swap")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind, stat err = %v", err)
	}

	if err := RestoreBackup(target); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if got := readFile(t, target); got != "old build" {
		t.Errorf("after restore target = %q, want the old build", got)
	}
	if HasBackup(target) {
		t.Error("backup should be consumed by the restore")
	}
	if err := RestoreBackup(target); !errors.Is(err, ErrNoBackup) {
		t.Errorf("second restore err = %v, want ErrNoBackup", err)
	}
}

func TestSwapRefusesSelfSwap(t *testing.T) {
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "build")
	if _, err := Swap(target, target); !errors.Is(err, ErrSameFilenames) {
		t.Errorf("err = %v, want ErrSameFilenames", err)
	}
}

func TestSwapMissingStagedLeavesTargetAlone(t *testing.T) {
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "old build")
	_, err := Swap(target, filepath.Join(t.TempDir(), "does-not-exist"))
	if !errors.Is(err, ErrSwap) {
		t.Fatalf("err = %v, want ErrSwap", err)
	}
	if got := readFile(t, target); got != "old build" {
		t.Errorf("target = %q, want it untouched", got)
	}
	if HasBackup(target) {
		t.Error("a failed swap must not leave a backup")
	}
}

func TestRemoveBackupIsIdempotent(t *testing.T) {
	binDir := t.TempDir()
	target := filepath.Join(binDir, "openrmm-agent")
	if err := RemoveBackup(target); err != nil {
		t.Fatalf("RemoveBackup with no backup: %v", err)
	}
	if err := os.WriteFile(BackupPath(target), []byte("old"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	if err := RemoveBackup(target); err != nil {
		t.Fatalf("RemoveBackup: %v", err)
	}
	if HasBackup(target) {
		t.Error("backup still present after RemoveBackup")
	}
}
