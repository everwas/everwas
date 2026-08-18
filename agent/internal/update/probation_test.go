package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeGuardStarts fakes what packaging/linux/agent-guard.sh appends: one
// unix timestamp per launch, written by the service manager BEFORE the agent
// runs.
func writeGuardStarts(t *testing.T, dir string, at ...time.Time) {
	t.Helper()
	var b strings.Builder
	for _, ts := range at {
		fmt.Fprintf(&b, "%d\n", ts.Unix())
	}
	if err := os.WriteFile(filepath.Join(dir, StartsFileName), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write guard starts: %v", err)
	}
}

// TestCheckAndRollbackCountsGuardStarts is the H1 regression: the crash
// counter used to be incremented only by the binary under test, so a build
// that dies before it can record a start left the counter at zero and the
// rollback never fired. The guard counts from outside the process.
func TestCheckAndRollbackCountsGuardStarts(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "everwas-agent", "new build")
	backup := BackupPath(target)
	if err := os.WriteFile(backup, []byte("old build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	clock := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	tr := &Tracker{StateDir: dir, Now: func() time.Time { return clock }}
	if err := tr.BeginUpdate("2.0.0", "1.0.0", target, backup); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	// Three launches the agent itself never lived to record, exactly what a
	// binary that panics in package init looks like from outside.
	writeGuardStarts(t, dir,
		clock.Add(-90*time.Second), clock.Add(-50*time.Second), clock.Add(-5*time.Second))

	rolled, err := tr.CheckAndRollback()
	if err != nil {
		t.Fatalf("CheckAndRollback: %v", err)
	}
	if !rolled {
		t.Fatal("a build that crash looped before it could count for itself was not rolled back")
	}
	if got := readFile(t, target); got != "old build" {
		t.Errorf("target = %q, want the previous build restored", got)
	}
	if !tr.IsDenied("2.0.0") {
		t.Error("a rolled back version must land on the denylist")
	}
}

// TestGuardRollbackIsReconciled covers the other half: the guard restored the
// binary before this process existed. The agent has to notice, record it and
// deny the version, or the server keeps sending the same bad build.
func TestGuardRollbackIsReconciled(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "everwas-agent", "old build")

	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", target, BackupPath(target)); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	// What the guard leaves behind: the probation is over, the version is
	// denied, and the binary is already the old one.
	if err := os.Remove(filepath.Join(dir, ProbationFileName)); err != nil {
		t.Fatalf("remove probation: %v", err)
	}
	if err := tr.Deny("2.0.0"); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	rolled, err := tr.CheckAndRollback()
	if err != nil {
		t.Fatalf("CheckAndRollback: %v", err)
	}
	if rolled {
		t.Error("the restore already happened, so this launch must not report a rollback")
	}
	st, err := tr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !st.RolledBack || st.Pending() {
		t.Errorf("state = %+v, want the guard's rollback recorded and nothing pending", st)
	}
	rep, err := tr.Report()
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Status != StatusRolledBack {
		t.Errorf("status = %s, want %s", rep.Status, StatusRolledBack)
	}
}

// TestBeginUpdateKeepsDenylist is the H3 regression: BeginUpdate used to
// write a fresh State{}, wiping the record of what had already been rolled
// back, so the fleet flapped against the same release forever.
func TestBeginUpdateKeepsDenylist(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.Save(State{Denied: []string{"2.0.0"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tr.BeginUpdate("2.1.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	st, err := tr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Denied) != 1 || st.Denied[0] != "2.0.0" {
		t.Errorf("denied = %v, want the earlier rollback remembered", st.Denied)
	}
	if !tr.IsDenied("2.0.0") {
		t.Error("IsDenied lost a version across an update")
	}
}

// TestBeginUpdateWritesProbationForTheGuard checks the plain text handshake
// with the shell guard. The guard cannot parse JSON, so if this file stops
// being written the external rollback silently stops working.
func TestBeginUpdateWritesProbationForTheGuard(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/usr/local/bin/everwas-agent", "/usr/local/bin/everwas-agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	body := readFile(t, filepath.Join(dir, ProbationFileName))
	for _, want := range []string{
		"version=2.0.0",
		"target=/usr/local/bin/everwas-agent",
		"backup=/usr/local/bin/everwas-agent.old",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("probation file is missing %q\n---\n%s", want, body)
		}
	}
}

// TestFinalizingIsNotHealthy is the H4 regression. The Windows fallback hands
// the swap to a helper process, so the running binary is still the old one:
// declaring it healthy is how a host stays behind while the console records
// the update as applied.
func TestFinalizingIsNotHealthy(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	if err := tr.BeginFinalize(4242); err != nil {
		t.Fatalf("BeginFinalize: %v", err)
	}

	rep, err := tr.Report()
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Status != StatusFinalizing {
		t.Errorf("status = %s, want %s", rep.Status, StatusFinalizing)
	}
	if rep.Terminal {
		t.Error("finalizing is not terminal: the swap has not happened yet")
	}

	if err := tr.MarkHealthy(); !errors.Is(err, ErrFinalizePending) {
		t.Errorf("MarkHealthy while finalizing = %v, want ErrFinalizePending", err)
	}
	st, err := tr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Healthy {
		t.Error("an unresolved finalizer must not leave a healthy record")
	}
}

// TestFinalizeFailureIsReported covers the finalizer that gave up waiting for
// the parent to exit. It used to exit 1 into the void.
func TestFinalizeFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	if err := tr.BeginFinalize(4242); err != nil {
		t.Fatalf("BeginFinalize: %v", err)
	}
	if err := tr.FinalizeOutcome(errors.New("pid 4242 is still running after 2m0s")); err != nil {
		t.Fatalf("FinalizeOutcome: %v", err)
	}

	rep, err := tr.Report()
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Status != StatusFinalizeFailed {
		t.Errorf("status = %s, want %s", rep.Status, StatusFinalizeFailed)
	}
	if !strings.Contains(rep.Detail, "still running") {
		t.Errorf("detail = %q, want the finalizer's own reason", rep.Detail)
	}
	// The swap never happened, so the guard must not be counting starts of a
	// binary that did not change.
	if _, err := os.Stat(filepath.Join(dir, ProbationFileName)); !os.IsNotExist(err) {
		t.Errorf("probation file should be gone after a failed finalize, stat err = %v", err)
	}
}

func TestFinalizeSuccessAllowsHealthy(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	if err := tr.BeginFinalize(4242); err != nil {
		t.Fatalf("BeginFinalize: %v", err)
	}
	if err := tr.FinalizeOutcome(nil); err != nil {
		t.Fatalf("FinalizeOutcome: %v", err)
	}
	if err := tr.MarkHealthy(); err != nil {
		t.Fatalf("MarkHealthy after a finished finalize: %v", err)
	}
}

// TestWatchNeedsEvidence is the H2 regression: probation used to end after
// sixty seconds of the process merely existing.
func TestWatchNeedsEvidence(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "everwas-agent", "new build")
	backup := BackupPath(target)
	if err := os.WriteFile(backup, []byte("old build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", target, backup); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}

	var calls atomic.Int32
	cfg := WatchConfig{
		StateDir:      dir,
		MinProbation:  10 * time.Millisecond,
		MaxProbation:  10 * time.Second,
		ProbeInterval: 5 * time.Millisecond,
		Probe: func(context.Context) error {
			// The shape of the bug: the process is alive and connected, but
			// no unit of work has completed.
			if calls.Add(1) < 4 {
				return errors.New("patchstate has never published")
			}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, cfg) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := tr.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if st.Healthy {
			if calls.Load() < 4 {
				t.Fatalf("healthy after %d probes, want evidence first", calls.Load())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Watch never confirmed the build even though the probe started succeeding")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := readFile(t, backup); got != "old build" {
		t.Errorf("backup = %q, want it kept after a healthy update", got)
	}
}

// TestWatchWithoutEvidenceStaysOnProbation is the case that used to be
// declared healthy at sixty seconds: an agent that starts, connects, and then
// does nothing at all.
func TestWatchWithoutEvidenceStaysOnProbation(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, WatchConfig{
			StateDir:      dir,
			MinProbation:  5 * time.Millisecond,
			MaxProbation:  time.Hour,
			ProbeInterval: 5 * time.Millisecond,
			Probe:         func(context.Context) error { return errors.New("no heartbeat round trip yet") },
		})
	}()

	time.Sleep(150 * time.Millisecond)
	st, err := tr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Healthy {
		t.Error("a build that never did a unit of work was declared healthy")
	}
	if !st.Pending() {
		t.Errorf("state = %+v, want the update still on probation", st)
	}
	cancel()
	<-done
}

// TestWatchExpiresUnproven checks the escape hatch: after MaxProbation the
// crash counter is disarmed, but the outcome is recorded honestly and the
// backup is still there.
func TestWatchExpiresUnproven(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, WatchConfig{
			StateDir:      dir,
			MinProbation:  5 * time.Millisecond,
			MaxProbation:  10 * time.Millisecond,
			ProbeInterval: 5 * time.Millisecond,
			Probe:         func(context.Context) error { return errors.New("never works") },
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		rep, err := tr.Report()
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		if rep.Status == StatusUnproven {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probation never expired, status = %s", rep.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// TestWatchWillNotConfirmAStalledFinalizer covers the H4 shape from the other
// side: the agent still running is the OLD binary, so a probe that succeeds
// says nothing about the build the server thinks was applied.
func TestWatchWillNotConfirmAStalledFinalizer(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	if err := tr.BeginFinalize(4242); err != nil {
		t.Fatalf("BeginFinalize: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, WatchConfig{
			StateDir:      dir,
			MinProbation:  5 * time.Millisecond,
			MaxProbation:  time.Hour,
			ProbeInterval: 5 * time.Millisecond,
			Probe:         func(context.Context) error { return nil },
		})
	}()

	time.Sleep(100 * time.Millisecond)
	rep, err := tr.Report()
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Status != StatusFinalizing || rep.Terminal {
		t.Errorf("report = %+v, want a non terminal finalizing status", rep)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Watch returned %v; a stalled finalizer is a report, not a task crash", err)
	}
}
