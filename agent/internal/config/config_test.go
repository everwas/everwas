package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EVERWAS_STATE_DIR", dir)

	want := &Config{
		ServerURL:   "https://rmm.example.com",
		AgentID:     "0198f6f2-0000-7000-8000-000000000001",
		AgentSecret: "s3cret",
		NATSURL:     "wss://rmm.example.com/nats",
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *got != *want {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, want)
	}
	if !got.Enrolled() {
		t.Error("Enrolled() = false after save/load")
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("EVERWAS_STATE_DIR", dir)

	cfg := &Config{AgentID: "a", AgentSecret: "b"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

func TestLoadMissingIsNotEnrolled(t *testing.T) {
	t.Setenv("EVERWAS_STATE_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if cfg.Enrolled() {
		t.Error("Enrolled() = true for missing state file")
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Error("LoadFrom garbage: want error, got nil")
	}
}
