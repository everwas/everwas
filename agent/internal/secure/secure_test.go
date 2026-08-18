package secure

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTheDirectoryIsNotReadableByOtherUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The Windows guarantee is an ACL, not a mode, and asserting on
		// os.FileMode there would pass while proving nothing. It is verified
		// against a real Windows host instead.
		t.Skip("mode bits do not express the Windows guarantee")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := MkdirAll(dir); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("mode = %o, want 700; this directory holds the agent credential "+
			"and the 802.1X private key", mode)
	}
}

func TestAnExistingLooseDirectoryIsRepaired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not express the Windows guarantee")
	}
	// The case that matters in the field: an agent installed before this
	// existed already has a world-readable directory full of secrets, and it
	// has to be fixed in place rather than needing a reinstall.
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAll(dir); err != nil {
		t.Fatalf("MkdirAll on an existing directory: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("mode = %o, want 700: an already-loose directory was left loose", mode)
	}
}

func TestItIsSafeToCallRepeatedly(t *testing.T) {
	// It runs on every agent start.
	dir := filepath.Join(t.TempDir(), "state")
	for i := range 3 {
		if err := MkdirAll(dir); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
