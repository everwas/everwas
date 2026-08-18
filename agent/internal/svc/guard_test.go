package svc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests run the guard script itself in a temp directory. Nothing here
// installs a service, touches systemd or launchd, or writes outside t.TempDir.

func TestRenderedGuardMatchesPackagedGuard(t *testing.T) {
	packaged, err := readIfExists(filepath.Join("..", "..", "packaging", "agent-guard.sh"))
	if err != nil {
		t.Fatalf("read packaged guard: %v", err)
	}
	if packaged == "" {
		t.Fatal("packaging/agent-guard.sh is missing, so a packaged install has no rollback guard")
	}
	if packaged != GuardScript {
		t.Error("packaging/agent-guard.sh differs from svc.GuardScript; a host installed from the deb would guard differently from one installed with `everwas-agent install`")
	}
}

// runGuard executes the guard the way a service manager would.
func runGuard(t *testing.T, stateDir string, args ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shell guard is unix only")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	script := filepath.Join(t.TempDir(), GuardName)
	if err := os.WriteFile(script, []byte(GuardScript), 0o755); err != nil {
		t.Fatalf("write guard: %v", err)
	}
	cmd := exec.Command(sh, append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "EVERWAS_STATE_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guard exited non-zero (%v), which would block startup: %s", err, out)
	}
	return string(out)
}

func writeProbation(t *testing.T, stateDir, version, target, backup string) {
	t.Helper()
	body := "version=" + version + "\nprevious=1.0.0\ntarget=" + target +
		"\nbackup=" + backup + "\nswapped_at=0\nfinalizing=false\n"
	if err := os.WriteFile(filepath.Join(stateDir, "update-probation"), []byte(body), 0o600); err != nil {
		t.Fatalf("write probation: %v", err)
	}
}

// TestGuardRestoresAMissingBinary is the case the in-process rollback can
// never cover: there is nothing to run, so nothing can count a crash.
func TestGuardRestoresAMissingBinary(t *testing.T) {
	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "everwas-agent")
	backup := target + ".old"
	if err := os.WriteFile(backup, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	writeProbation(t, stateDir, "2.0.0", target, backup)

	runGuard(t, stateDir, "check", target)

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("the guard did not restore the binary: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("restored binary is not executable, mode = %v", fi.Mode())
	}
	denied, err := os.ReadFile(filepath.Join(stateDir, "update-denied"))
	if err != nil {
		t.Fatalf("read denylist: %v", err)
	}
	if !strings.Contains(string(denied), "2.0.0") {
		t.Errorf("denylist = %q, want the rolled back version so the server stops resending it", denied)
	}
}

// TestGuardRestoresANonExecutableBinary covers a truncated or wrong-mode
// download: the file exists, so a "does it exist" check would pass.
func TestGuardRestoresANonExecutableBinary(t *testing.T) {
	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "everwas-agent")
	backup := target + ".old"
	if err := os.WriteFile(target, []byte("half a download"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(backup, []byte("good build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	writeProbation(t, stateDir, "2.0.0", target, backup)

	runGuard(t, stateDir, "check", target)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "good build" {
		t.Errorf("target = %q, want the previous build restored", got)
	}
}

// TestGuardRestoresAfterRepeatedStarts is the crash loop the agent cannot
// count for itself: the binary starts, dies before it records anything, and
// starts again.
func TestGuardRestoresAfterRepeatedStarts(t *testing.T) {
	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "everwas-agent")
	backup := target + ".old"
	if err := os.WriteFile(target, []byte("crashes on start"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(backup, []byte("good build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	writeProbation(t, stateDir, "2.0.0", target, backup)

	for i := 0; i < 2; i++ {
		runGuard(t, stateDir, "check", target)
		if got, _ := os.ReadFile(target); string(got) != "crashes on start" {
			t.Fatalf("rolled back after %d starts, too eager", i+1)
		}
	}
	runGuard(t, stateDir, "check", target)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "good build" {
		t.Errorf("target = %q, want the previous build restored after three starts in the window", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "update-probation")); !os.IsNotExist(err) {
		t.Errorf("probation should be over after a rollback, stat err = %v", err)
	}
}

// TestGuardLeavesAHealthyAgentAlone is the case that runs on every start of
// every host in the fleet. It has to be a no-op.
func TestGuardLeavesAHealthyAgentAlone(t *testing.T) {
	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "everwas-agent")
	backup := target + ".old"
	if err := os.WriteFile(target, []byte("current build"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(backup, []byte("previous build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	// No probation file: no update in flight.
	for i := 0; i < 5; i++ {
		runGuard(t, stateDir, "check", target)
	}
	if got, _ := os.ReadFile(target); string(got) != "current build" {
		t.Errorf("target = %q, want it untouched outside an update", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "update-starts")); !os.IsNotExist(err) {
		t.Error("the guard must not count starts outside an update")
	}
}

// TestGuardDoesNotCountWhileFinalizing keeps the Windows-style handoff from
// looking like a crash loop: the running binary has not been replaced yet, so
// its restarts say nothing about the new build.
func TestGuardDoesNotCountWhileFinalizing(t *testing.T) {
	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "everwas-agent")
	backup := target + ".old"
	if err := os.WriteFile(target, []byte("current build"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.WriteFile(backup, []byte("previous build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	body := "version=2.0.0\ntarget=" + target + "\nbackup=" + backup + "\nswapped_at=0\nfinalizing=true\n"
	if err := os.WriteFile(filepath.Join(stateDir, "update-probation"), []byte(body), 0o600); err != nil {
		t.Fatalf("write probation: %v", err)
	}

	for i := 0; i < 5; i++ {
		runGuard(t, stateDir, "check", target)
	}
	if got, _ := os.ReadFile(target); string(got) != "current build" {
		t.Errorf("target = %q, want it untouched while a finalizer is outstanding", got)
	}
}

// TestGuardExecModeRunsTheAgent covers the launchd path, where the guard is
// what launchd starts and the agent has to end up with the same pid.
func TestGuardExecModeRunsTheAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell guard is unix only")
	}
	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "everwas-agent")
	marker := filepath.Join(binDir, "ran")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > " + marker + "\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	runGuard(t, stateDir, "exec", target, "run")

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := os.ReadFile(marker)
		if err == nil {
			if string(got) != "run" {
				t.Errorf("agent saw args %q, want run", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the guard never execed the agent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGuardStateDirMatchesConfigDir pins the handshake that is easiest to
// break by accident. The guard looks for the probation record in the agent's
// state directory; if the two disagree it finds nothing, counts nothing, and
// fails silently in the one situation it exists for.
func TestGuardStateDirMatchesConfigDir(t *testing.T) {
	for _, want := range []string{
		`STATE_DIR="/etc/everwas"`,                         // config.Dir() on linux as root
		`STATE_DIR="/Library/Application Support/Everwas"`, // config.Dir() on darwin
		`${EVERWAS_STATE_DIR:-}`,                           // the override the unit and plist set
	} {
		if !strings.Contains(GuardScript, want) {
			t.Errorf("guard is missing %s, so it would read a different state dir from the agent", want)
		}
	}
}
