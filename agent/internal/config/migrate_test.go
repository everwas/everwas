package config

import (
	"os"
	"path/filepath"
	"testing"
)

// enrolledAt writes a state directory that looks like a real enrolled agent:
// the credential, the 802.1X material, and the schedule cache.
func enrolledAt(t *testing.T, dir, agentID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "netcert"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(FileName, `{"agent_id":"`+agentID+`","agent_secret":"s","server_url":"https://x","nats_url":"wss://x"}`)
	write(filepath.Join("netcert", "network.key"), "KEY")
	write("schedule.json", "{}")
}

func TestAnEnrolledAgentKeepsItsIdentityAcrossTheRename(t *testing.T) {
	// The failure this exists to prevent: the renamed binary looks in the new
	// directory, finds nothing, reports "not enrolled" and exits. The device
	// cannot be fixed remotely either, because the credential it would
	// authenticate with is the thing it can no longer find.
	root := t.TempDir()
	legacy := filepath.Join(root, "openrmm")
	current := filepath.Join(root, "everwas")
	enrolledAt(t, legacy, "01a00b45-0e50-78c8-b572-8b8fbc272ad1")

	moved, err := migrateState(legacy, current)
	if err != nil {
		t.Fatalf("migrateState: %v", err)
	}
	if !moved {
		t.Fatal("nothing was migrated, so this agent would report itself unenrolled")
	}
	if _, err := os.Stat(filepath.Join(current, FileName)); err != nil {
		t.Errorf("the credential did not arrive at the new path: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the old directory is still there, so a later run could migrate twice")
	}
}

func TestTheWholeDirectoryMovesNotJustTheCredential(t *testing.T) {
	// Moving agent.json alone leaves the machine enrolled while silently
	// dropping its 802.1X identity and its schedule, which is a subtler
	// version of the same failure and harder to diagnose because the agent
	// comes up looking healthy.
	root := t.TempDir()
	legacy := filepath.Join(root, "openrmm")
	current := filepath.Join(root, "everwas")
	enrolledAt(t, legacy, "device-a")

	if _, err := migrateState(legacy, current); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		FileName,
		filepath.Join("netcert", "network.key"),
		"schedule.json",
	} {
		if _, err := os.Stat(filepath.Join(current, rel)); err != nil {
			t.Errorf("%s was left behind: %v", rel, err)
		}
	}
}

func TestAFreshInstallUnderTheNewNameIsNotOverwritten(t *testing.T) {
	// Not merely "already migrated". This is somebody who enrolled fresh under
	// the new name while an old directory happened to still exist. Migrating
	// would replace a working identity with a stale one, and the machine would
	// authenticate as a device somebody else may since have retired.
	root := t.TempDir()
	legacy := filepath.Join(root, "openrmm")
	current := filepath.Join(root, "everwas")
	enrolledAt(t, legacy, "stale-old-identity")
	enrolledAt(t, current, "the-real-current-identity")

	moved, err := migrateState(legacy, current)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("overwrote a working identity with a stale one")
	}
	raw, err := os.ReadFile(filepath.Join(current, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(raw), "the-real-current-identity") {
		t.Errorf("the current identity was replaced: %s", raw)
	}
	// And the old directory is left for a human rather than deleted, since
	// deleting somebody's only copy of an identity is not ours to do.
	if _, err := os.Stat(legacy); err != nil {
		t.Error("the legacy directory was removed rather than left alone")
	}
}

func TestAFreshMachineMigratesNothingAndSaysNothing(t *testing.T) {
	// The common case by a wide margin. It must be silent: an install-time log
	// line about migrating from a project name the operator has never heard of
	// is a support ticket.
	root := t.TempDir()
	moved, err := migrateState(filepath.Join(root, "openrmm"), filepath.Join(root, "everwas"))
	if err != nil {
		t.Errorf("a fresh machine reported an error: %v", err)
	}
	if moved {
		t.Error("claimed to migrate something on a machine with no state at all")
	}
}

func TestALegacyDirectoryWithNoCredentialIsNotMigrated(t *testing.T) {
	// A leftover empty directory, or one holding only logs. Moving it would
	// shadow the real state directory with junk.
	root := t.TempDir()
	legacy := filepath.Join(root, "openrmm")
	current := filepath.Join(root, "everwas")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	moved, err := migrateState(legacy, current)
	if err != nil || moved {
		t.Errorf("moved=%v err=%v, want a directory with no credential to be ignored", moved, err)
	}
}

func TestMigratingTwiceIsHarmless(t *testing.T) {
	// It runs on every agent start.
	root := t.TempDir()
	legacy := filepath.Join(root, "openrmm")
	current := filepath.Join(root, "everwas")
	enrolledAt(t, legacy, "device-a")

	for i := range 3 {
		if _, err := migrateState(legacy, current); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(current, FileName)); err != nil {
		t.Errorf("the credential went missing across repeated runs: %v", err)
	}
}

func TestAnExplicitStateDirDisablesLegacyHunting(t *testing.T) {
	// An operator who points the state dir somewhere explicitly is telling us
	// where it is. Hunting for an older directory anyway is how a dev machine
	// picks up a stale identity from a previous experiment.
	t.Setenv("EVERWAS_STATE_DIR", t.TempDir())
	if got := legacyDir(); got != "" {
		t.Errorf("legacyDir() = %q with an explicit state dir set, want empty", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
