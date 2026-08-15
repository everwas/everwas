package sched

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func testScheduler(t *testing.T, run RunFunc) *Scheduler {
	t.Helper()
	return New("agent-1", t.TempDir(), run, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func daily(entryID string, graceS int) Entry {
	return Entry{
		EntryID:       entryID,
		Cron:          "0 3 * * *",
		TZ:            "UTC",
		Kind:          "script.run",
		Payload:       json.RawMessage(`{"shell":"bash","body":"echo hi"}`),
		MisfireGraceS: graceS,
		Enabled:       true,
	}
}

func TestSyncPersistsAndReloads(t *testing.T) {
	s := testScheduler(t, nil)
	doc := Document{ScheduleVersion: 7, Entries: []Entry{daily("nightly", 3600)}}

	version, err := s.Sync(doc)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if version != 7 || s.Version() != 7 {
		t.Fatalf("version = %d / %d, want 7", version, s.Version())
	}

	fi, err := os.Stat(s.path)
	if err != nil {
		t.Fatalf("stat schedule: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("schedule.json mode = %o, want 600", perm)
	}
	if filepath.Base(s.path) != FileName {
		t.Errorf("schedule file = %s, want %s", filepath.Base(s.path), FileName)
	}

	reloaded := New("agent-1", filepath.Dir(s.path), nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Version() != 7 {
		t.Errorf("reloaded version = %d, want 7", reloaded.Version())
	}
	if entries := reloaded.Entries(); len(entries) != 1 || entries[0].EntryID != "nightly" {
		t.Errorf("reloaded entries = %+v", entries)
	}
}

func TestLoadMissingFileIsEmptySchedule(t *testing.T) {
	s := testScheduler(t, nil)
	if err := s.Load(); err != nil {
		t.Fatalf("Load on a fresh agent: %v", err)
	}
	if s.Version() != 0 || len(s.Entries()) != 0 {
		t.Errorf("fresh agent has version %d and %d entries", s.Version(), len(s.Entries()))
	}
}

// TestSyncDoesNotBackdateNewEntries: a nightly job added at noon must not
// immediately "catch up" the 3am run it was never scheduled for.
func TestSyncDoesNotBackdateNewEntries(t *testing.T) {
	s := testScheduler(t, nil)
	if _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{daily("nightly", 86400)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	last, ok := s.lastFired("nightly")
	if !ok {
		t.Fatal("new entry has no last-fired mark")
	}
	if time.Since(last) > time.Minute {
		t.Errorf("last fired = %s, want ~now", last)
	}

	s.catchUp(context.Background(), time.Now())
	if got := len(s.pq); got != 0 {
		t.Errorf("catch-up queued %d runs for a brand new entry", got)
	}
}

// TestSyncKeepsFireHistory: re-syncing a schedule must not lose what we
// already ran, or every schedule push would re-trigger the same jobs.
func TestSyncKeepsFireHistory(t *testing.T) {
	s := testScheduler(t, nil)
	fired := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	if _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{daily("nightly", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	s.markFired("nightly", fired)

	if _, err := s.Sync(Document{ScheduleVersion: 2, Entries: []Entry{daily("nightly", 60)}}); err != nil {
		t.Fatalf("re-Sync: %v", err)
	}
	got, ok := s.lastFired("nightly")
	if !ok || !got.Equal(fired) {
		t.Errorf("last fired = %v (%v), want %v", got, ok, fired)
	}
}

func TestSyncDropsRemovedEntries(t *testing.T) {
	s := testScheduler(t, nil)
	if _, err := s.Sync(Document{ScheduleVersion: 1,
		Entries: []Entry{daily("a", 60), daily("b", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := s.Sync(Document{ScheduleVersion: 2, Entries: []Entry{daily("a", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, ok := s.lastFired("b"); ok {
		t.Error("removed entry kept its fire history")
	}
}

func TestCatchUpDecisions(t *testing.T) {
	// A fixed clock: with a per-minute entry the most recent missed fire is
	// always seconds old, so the grace decision has to be driven by the
	// entry's own cadence, not by wall time.
	now := time.Date(2026, 8, 15, 12, 0, 30, 0, time.UTC)
	minutely := func(graceS int) Entry {
		e := daily("frequent", graceS)
		e.Cron = "* * * * *"
		return e
	}
	tests := []struct {
		name      string
		entry     Entry
		lastFired time.Time
		wantQueue int
	}{
		{"recent miss inside grace runs", minutely(3600), now.Add(-10 * time.Minute), 1},
		{"nothing missed", minutely(3600), now.Add(-2 * time.Second), 0},
		{"zero grace never catches up", minutely(0), now.Add(-10 * time.Minute), 0},
		{"disabled entries are ignored", disabled(minutely(3600)), now.Add(-10 * time.Minute), 0},
		// Daily at 03:00, three days down: the newest miss is nine hours
		// old, far outside a one minute grace.
		{"stale daily miss is skipped", daily("nightly", 60), now.AddDate(0, 0, -3), 0},
		{"daily miss inside a generous grace runs", daily("nightly", 12*3600), now.AddDate(0, 0, -3), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testScheduler(t, nil)
			if _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{tt.entry}}); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			s.markFired(tt.entry.EntryID, tt.lastFired)

			s.catchUp(context.Background(), now)
			if got := len(s.pq); got != tt.wantQueue {
				t.Fatalf("queued %d catch-up runs, want %d", got, tt.wantQueue)
			}
			if tt.wantQueue > 0 {
				it := s.pq[0]
				if it.at.Before(now) || it.at.After(now.Add(MisfireJitterS*time.Second)) {
					t.Errorf("catch-up scheduled at %s, want within %ds of %s",
						it.at, MisfireJitterS, now)
				}
				if !it.base.Before(now) {
					t.Errorf("catch-up base %s is not in the past", it.base)
				}
			}
			// A skipped miss must be recorded, or it is re-evaluated forever.
			if tt.wantQueue == 0 && tt.entry.Enabled {
				if last, _ := s.lastFired(tt.entry.EntryID); last.Before(tt.lastFired) {
					t.Errorf("last fired went backwards: %s", last)
				}
			}
		})
	}
}

func TestFireDueDispatchesJobID(t *testing.T) {
	type call struct {
		jobID   string
		entryID string
		fireAt  time.Time
	}
	calls := make(chan call, 4)
	s := testScheduler(t, func(_ context.Context, jobID string, e Entry, at time.Time) {
		calls <- call{jobID, e.EntryID, at}
	})
	if _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{daily("nightly", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	base := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	s.pq = fireHeap{{entryID: "nightly", base: base, at: base}}

	s.fireDue(context.Background(), time.Now())
	select {
	case got := <-calls:
		want := "sched:nightly:" + strconv.FormatInt(base.Unix(), 10)
		if got.jobID != want {
			t.Errorf("job id = %q, want %q", got.jobID, want)
		}
		if !got.fireAt.Equal(base) {
			t.Errorf("fire time = %s, want the unjittered %s", got.fireAt, base)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled entry never dispatched")
	}
	// The next occurrence must be queued, or the entry fires once and dies.
	if len(s.pq) != 1 {
		t.Errorf("queue holds %d items after firing, want the next occurrence", len(s.pq))
	}
	if last, ok := s.lastFired("nightly"); !ok || !last.Equal(base) {
		t.Errorf("last fired = %v, want %s", last, base)
	}
}

func TestItemForAppliesDeterministicJitter(t *testing.T) {
	s := testScheduler(t, nil)
	e := daily("nightly", 60)
	e.JitterS = 1800
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)

	it, ok := s.itemFor(e, now)
	if !ok {
		t.Fatal("itemFor failed on a valid entry")
	}
	offset := it.at.Sub(it.base)
	if offset != Jitter("agent-1", "nightly", 1800) {
		t.Errorf("offset = %s, want the deterministic jitter", offset)
	}
	if offset < 0 || offset >= 1800*time.Second {
		t.Errorf("offset %s outside the jitter window", offset)
	}
	again, _ := s.itemFor(e, now)
	if !again.at.Equal(it.at) {
		t.Error("two calls produced different fire times")
	}
}

func TestItemForRejectsBadEntries(t *testing.T) {
	s := testScheduler(t, nil)
	bad := []Entry{
		{EntryID: "a", Cron: "not a cron", Enabled: true},
		{EntryID: "b", Cron: "0 3 * * *", TZ: "Mars/Olympus", Enabled: true},
	}
	for _, e := range bad {
		if _, ok := s.itemFor(e, time.Now()); ok {
			t.Errorf("entry %+v should not schedule", e)
		}
	}
}

func TestScheduleUsesEntryTimezone(t *testing.T) {
	s := testScheduler(t, nil)
	e := daily("nightly", 60)
	e.TZ = "America/Denver"
	// 2026-08-15 08:00 UTC is 02:00 in Denver, so the 03:00 local fire is
	// an hour away rather than 19 hours away.
	now := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	it, ok := s.itemFor(e, now)
	if !ok {
		t.Fatal("itemFor failed")
	}
	if delta := it.base.Sub(now); delta != time.Hour {
		t.Errorf("next fire in %s, want 1h (timezone not applied?)", delta)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s := testScheduler(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil, want the context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored context cancellation")
	}
}

func TestJobID(t *testing.T) {
	base := time.Unix(1755225600, 0).UTC()
	if got := JobID("nightly-scan", base); got != "sched:nightly-scan:1755225600" {
		t.Errorf("JobID = %q", got)
	}
}

func disabled(e Entry) Entry {
	e.Enabled = false
	return e
}
