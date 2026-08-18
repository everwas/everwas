package sched

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/everwas/everwas/agent/internal/wire"
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

	version, _, err := s.Sync(doc)
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
	if _, _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{daily("nightly", 86400)}}); err != nil {
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
	if _, _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{daily("nightly", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	s.markFired("nightly", fired)

	if _, _, err := s.Sync(Document{ScheduleVersion: 2, Entries: []Entry{daily("nightly", 60)}}); err != nil {
		t.Fatalf("re-Sync: %v", err)
	}
	got, ok := s.lastFired("nightly")
	if !ok || !got.Equal(fired) {
		t.Errorf("last fired = %v (%v), want %v", got, ok, fired)
	}
}

func TestSyncDropsRemovedEntries(t *testing.T) {
	s := testScheduler(t, nil)
	if _, _, err := s.Sync(Document{ScheduleVersion: 1,
		Entries: []Entry{daily("a", 60), daily("b", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, _, err := s.Sync(Document{ScheduleVersion: 2, Entries: []Entry{daily("a", 60)}}); err != nil {
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
			if _, _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{tt.entry}}); err != nil {
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
	if _, _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{daily("nightly", 60)}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Due a moment ago: an ordinary on-time fire, not a misfire.
	base := time.Now().Add(-5 * time.Second).Truncate(time.Second)
	s.pq = fireHeap{{entryID: "nightly", base: base, at: base}}

	s.fireDue(context.Background(), time.Now())
	select {
	case got := <-calls:
		want := JobID("nightly", base)
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

// TestFireDueAppliesTheMisfireGrace is the regression for a grace that was
// only enforced at startup. Go timers are monotonic: a laptop suspended at
// 22:00 and resumed at 09:00 finds the 22:30 timer already ready and fired
// the nightly job immediately, in front of the user, even with
// misfire_grace_s: 0 meaning "this is worthless late".
func TestFireDueAppliesTheMisfireGrace(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		graceS  int
		lateBy  time.Duration
		wantRun bool
	}{
		{"an on-time fire runs", 0, 2 * time.Second, true},
		{"eleven hours late with no grace is skipped", 0, 11 * time.Hour, false},
		{"eleven hours late outside a one hour grace is skipped", 3600, 11 * time.Hour, false},
		{"ten minutes late inside a one hour grace runs", 3600, 10 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fired := make(chan string, 1)
			s := testScheduler(t, func(_ context.Context, jobID string, _ Entry, _ time.Time) {
				fired <- jobID
			})
			entry := daily("nightly", tt.graceS)
			if _, _, err := s.Sync(Document{ScheduleVersion: 1, Entries: []Entry{entry}}); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			base := now.Add(-tt.lateBy).Truncate(time.Second)
			s.pq = fireHeap{{entryID: "nightly", base: base, at: base}}

			s.fireDue(context.Background(), now)
			select {
			case jobID := <-fired:
				if !tt.wantRun {
					t.Fatalf("a fire %s late ran anyway: %s", tt.lateBy, jobID)
				}
			case <-time.After(time.Second):
				if tt.wantRun {
					t.Fatal("a fire inside its grace window never ran")
				}
			}
			// Skipped or not, the fire must be recorded, or the same missed
			// occurrence is re-evaluated on every pass.
			if last, ok := s.lastFired("nightly"); !ok || last.Before(base) {
				t.Errorf("last fired = %v, want at least %s", last, base)
			}
			// The next occurrence is queued either way; an entry must not
			// stop scheduling itself because one fire was late.
			if len(s.pq) != 1 {
				t.Errorf("queue holds %d items, want the next occurrence", len(s.pq))
			}
		})
	}
}

// TestSyncRejectsUnschedulableEntries is the regression for entries that
// were persisted and answered `accepted: true`, then parsed hours later,
// logged as bad and skipped forever. The server went on believing the
// schedule was in force.
func TestSyncRejectsUnschedulableEntries(t *testing.T) {
	s := testScheduler(t, nil)
	badCron := daily("bad-cron", 60)
	badCron.Cron = "not a cron"
	badTZ := daily("bad-tz", 60)
	badTZ.TZ = "Mars/Olympus"

	version, rejected, err := s.Sync(Document{
		ScheduleVersion: 5,
		Entries:         []Entry{daily("nightly", 60), badCron, badTZ},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if version != 5 {
		t.Errorf("version = %d, want 5", version)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected = %+v, want the two unschedulable entries", rejected)
	}
	byID := map[string]string{}
	for _, r := range rejected {
		byID[r.EntryID] = r.Reason
	}
	if !strings.Contains(byID["bad-cron"], "cron") {
		t.Errorf("bad-cron reason = %q, want it to name the cron", byID["bad-cron"])
	}
	if !strings.Contains(byID["bad-tz"], "timezone") {
		t.Errorf("bad-tz reason = %q, want it to name the timezone", byID["bad-tz"])
	}
	// The good entry still applies, and the rejected ones are not cached as
	// though they were going to run.
	entries := s.Entries()
	if len(entries) != 1 || entries[0].EntryID != "nightly" {
		t.Errorf("cached entries = %+v, want only the schedulable one", entries)
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

// TestJobIDMatchesTheServersDerivation pins the exact bytes both sides must
// agree on. The vector below is Python's
// uuid5(uuid5(NAMESPACE_DNS, "schedule.everwas.invalid"), "nightly-scan:1755225600"),
// which is what everwas.services.schedules.scheduled_job_id computes. If this
// test and its Python twin ever disagree, scheduled results arrive for a run
// id the server never created and are dropped as "unknown run".
func TestJobIDMatchesTheServersDerivation(t *testing.T) {
	base := time.Unix(1755225600, 0).UTC()
	const want = "11bea7a9-2e3c-5b06-90a7-0e4c7ba7c2f1"
	if got := JobID("nightly-scan", base); got != want {
		t.Errorf("JobID = %q, want %q", got, want)
	}
}

// TestJobIDIsAUUID: the server parses every job id as a UUID to find the row
// a result belongs to. An id that is not one is not "ugly", it is a result
// that gets logged as unknown and thrown away.
func TestJobIDIsAUUID(t *testing.T) {
	id := JobID("nightly-scan", time.Unix(1755225600, 0))
	if err := wire.CheckIdentifier("job_id", id); err != nil {
		t.Errorf("job id is not a legal subject token: %v", err)
	}
	if len(id) != 36 {
		t.Errorf("job id %q is not a UUID", id)
	}
	for _, i := range []int{8, 13, 18, 23} {
		if id[i] != '-' {
			t.Errorf("job id %q is not a UUID", id)
		}
	}
	if id[14] != '5' {
		t.Errorf("job id %q is not version 5, so it is not derived from the entry", id)
	}
}

// TestJobIDIsStableForTheSameFire is what makes a doubly-reported run
// idempotent server-side: the same entry and the same unjittered fire time
// must always produce the same id, and a different fire must not.
func TestJobIDIsStableForTheSameFire(t *testing.T) {
	base := time.Unix(1755225600, 0)
	if JobID("nightly", base) != JobID("nightly", base) {
		t.Error("the same fire produced two ids, so a retry would create a second run")
	}
	if JobID("nightly", base) == JobID("nightly", base.Add(time.Hour)) {
		t.Error("two different fires share an id, so the second overwrites the first")
	}
	if JobID("nightly", base) == JobID("weekly", base) {
		t.Error("two different entries share an id")
	}
}

func disabled(e Entry) Entry {
	e.Enabled = false
	return e
}

// TestSyncRejectsUnusableEntryIDs is the regression for the third
// interpolation site. An entry id becomes part of the job id
// (sched:{entry_id}:{ts}), and the job id becomes three publish subjects, so
// a wildcard smuggled in through a schedule sync would take the connection
// down hours later, when the entry fired, with nothing pointing back at the
// sync that caused it.
func TestSyncRejectsUnusableEntryIDs(t *testing.T) {
	for _, id := range []string{">", "*", "", "..", "a b", "nightly>"} {
		t.Run(strconv.Quote(id), func(t *testing.T) {
			s := testScheduler(t, nil)
			good := Document{ScheduleVersion: 3, Entries: []Entry{daily("nightly", 3600)}}
			if _, _, err := s.Sync(good); err != nil {
				t.Fatalf("Sync(good): %v", err)
			}

			bad := Document{ScheduleVersion: 4, Entries: []Entry{daily("nightly", 3600), daily(id, 3600)}}
			_, _, syncErr := s.Sync(bad)
			if syncErr == nil {
				t.Fatalf("Sync accepted entry_id %q", id)
			}
			if !errors.Is(syncErr, wire.ErrInvalidIdentifier) {
				t.Errorf("Sync(%q) = %v, want it to wrap wire.ErrInvalidIdentifier", id, syncErr)
			}
			// The rejection is whole-document: the previous schedule survives
			// rather than being half replaced by a payload we refused.
			if got := s.Version(); got != 3 {
				t.Errorf("schedule_version = %d after a refused sync, want 3", got)
			}
			if entries := s.Entries(); len(entries) != 1 || entries[0].EntryID != "nightly" {
				t.Errorf("cached entries = %v, want the previous schedule untouched", entries)
			}
		})
	}
}
