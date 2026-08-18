package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldRollbackDecision(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	base := State{PendingVersion: "2.0.0", Target: "/usr/local/bin/everwas-agent", Backup: "/usr/local/bin/everwas-agent.old"}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	withStarts := func(s State, starts ...time.Time) State {
		s.Starts = starts
		return s
	}

	cases := []struct {
		name     string
		st       State
		external []time.Time
		want     bool
	}{
		{"no update pending", State{}, nil, false},
		{"first start", withStarts(base, ago(10*time.Second)), nil, false},
		{"one crash", withStarts(base, ago(40*time.Second), ago(10*time.Second)), nil, false},
		{"two crashes in window", withStarts(base, ago(90*time.Second), ago(50*time.Second), ago(5*time.Second)), nil, true},
		{"crashes outside window", withStarts(base, ago(10*time.Minute), ago(9*time.Minute), ago(5*time.Second)), nil, false},
		{"already healthy", withStarts(func() State { s := base; s.Healthy = true; return s }(),
			ago(90*time.Second), ago(50*time.Second), ago(5*time.Second)), nil, false},
		{"already rolled back", withStarts(func() State { s := base; s.RolledBack = true; return s }(),
			ago(90*time.Second), ago(50*time.Second), ago(5*time.Second)), nil, false},
		{"no backup recorded", withStarts(func() State { s := base; s.Backup = ""; return s }(),
			ago(90*time.Second), ago(50*time.Second), ago(5*time.Second)), nil, false},
		{"clock skew, starts in the future", withStarts(base, now.Add(time.Hour), now.Add(2*time.Hour), now.Add(3*time.Hour)), nil, false},

		// The build that never runs is the one that needs rolling back most:
		// a wrong-architecture binary, a panic in package init, or a config
		// format the new build cannot parse never reaches RecordStart, so its
		// own counter stays at zero forever. The external guard counts those.
		{"guard counted the starts the binary could not", base,
			[]time.Time{ago(90 * time.Second), ago(50 * time.Second), ago(5 * time.Second)}, true},
		{"guard counts do not add to the binary's own", withStarts(base, ago(50*time.Second), ago(5*time.Second)),
			[]time.Time{ago(50 * time.Second), ago(5 * time.Second)}, false},
	}
	for _, c := range cases {
		if got := shouldRollback(c.st, c.external, now); got != c.want {
			t.Errorf("%s: shouldRollback = %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeBinary writes a stand-in executable and returns its path.
func fakeBinary(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestCheckAndRollbackRestoresPreviousBinary(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "everwas-agent", "new build")
	backup := BackupPath(target)
	if err := os.WriteFile(backup, []byte("old build"), 0o755); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	clock := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tr := &Tracker{StateDir: dir, Now: func() time.Time { return clock }}
	if err := tr.BeginUpdate("2.0.0", "1.0.0", target, backup); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}

	// Launch one: the normal post-update start.
	rolled, err := tr.CheckAndRollback()
	if err != nil || rolled {
		t.Fatalf("start 1: rolled=%v err=%v", rolled, err)
	}
	// Launch two: first crash restart.
	clock = clock.Add(20 * time.Second)
	rolled, err = tr.CheckAndRollback()
	if err != nil || rolled {
		t.Fatalf("start 2: rolled=%v err=%v", rolled, err)
	}
	// Launch three: second crash restart, inside the two minute window.
	clock = clock.Add(20 * time.Second)
	rolled, err = tr.CheckAndRollback()
	if err != nil {
		t.Fatalf("start 3: %v", err)
	}
	if !rolled {
		t.Fatal("start 3: expected a rollback")
	}
	if got := readFile(t, target); got != "old build" {
		t.Errorf("target contents = %q, want the previous build", got)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("backup should be consumed by the restore, stat err = %v", err)
	}

	// A fourth launch must not loop: there is nothing left to restore.
	clock = clock.Add(20 * time.Second)
	rolled, err = tr.CheckAndRollback()
	if err != nil || rolled {
		t.Fatalf("start 4: rolled=%v err=%v", rolled, err)
	}
}

func TestCheckAndRollbackNoopWithoutPendingUpdate(t *testing.T) {
	dir := t.TempDir()
	tr := NewTracker(dir)
	rolled, err := tr.CheckAndRollback()
	if err != nil {
		t.Fatalf("CheckAndRollback: %v", err)
	}
	if rolled {
		t.Fatal("rolled back with no update in flight")
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("a quiet start should not create a state file, stat err = %v", err)
	}
}

// TestMarkHealthyKeepsBackup pins the recovery path. Deleting the previous
// binary at the end of probation makes every defect that shows up later
// unrecoverable: there is no other copy of a working agent on the host, and
// the broken one is the thing that would have to fetch it.
func TestMarkHealthyKeepsBackup(t *testing.T) {
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
	if _, err := tr.RecordStart(); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}
	if err := tr.MarkHealthy(); err != nil {
		t.Fatalf("MarkHealthy: %v", err)
	}

	if got := readFile(t, backup); got != "old build" {
		t.Errorf("backup = %q, want the previous build kept for recovery", got)
	}
	// The guard's files go away with the probation, so a restart weeks later
	// is not counted against an update that is long over.
	for _, name := range []string{ProbationFileName, StartsFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone once healthy, stat err = %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, healthMarkerName)); err != nil {
		t.Errorf("health marker missing: %v", err)
	}
	st, err := tr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !st.Healthy || st.Pending() {
		t.Errorf("state after MarkHealthy = %+v, want healthy and not pending", st)
	}

	// Crash counting stops once healthy, no matter how many restarts follow.
	for i := 0; i < 5; i++ {
		rolled, err := tr.CheckAndRollback()
		if err != nil || rolled {
			t.Fatalf("restart %d after healthy: rolled=%v err=%v", i, rolled, err)
		}
	}
}

func TestLoadCorruptStateIsHarmless(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := NewTracker(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Pending() {
		t.Errorf("corrupt state should read as no update in flight, got %+v", st)
	}
}

func TestRecordStartTrimsHistory(t *testing.T) {
	dir := t.TempDir()
	clock := time.Now()
	tr := &Tracker{StateDir: dir, Now: func() time.Time { return clock }}
	if err := tr.BeginUpdate("2.0.0", "1.0.0", "/bin/agent", "/bin/agent.old"); err != nil {
		t.Fatalf("BeginUpdate: %v", err)
	}
	for i := 0; i < maxStartsTracked+10; i++ {
		clock = clock.Add(time.Hour) // far outside the crash window
		if _, err := tr.RecordStart(); err != nil {
			t.Fatalf("RecordStart %d: %v", i, err)
		}
	}
	st, err := tr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Starts) != maxStartsTracked {
		t.Errorf("tracked %d starts, want %d", len(st.Starts), maxStartsTracked)
	}
}
