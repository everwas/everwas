// Package sched keeps a local copy of the server's schedule so jobs still
// fire while the agent is offline. Fire times carry a deterministic jitter
// derived from the agent and entry ids, so a fleet spreads itself the same
// way every night instead of re-rolling the spread on each run.
package sched

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
	_ "time/tzdata" // CGO_ENABLED=0 builds have no system tz database

	"github.com/robfig/cron/v3"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

// parser accepts standard 5-field cron plus @daily-style descriptors.
var parser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// RunFunc executes one scheduled entry. jobID is
// sched:{entry_id}:{unix_base_fire_ts} — built from the unjittered time so
// the server can derive the same id and dedup a doubly-reported run.
type RunFunc func(ctx context.Context, jobID string, entry Entry, fireAt time.Time)

// Scheduler owns the schedule cache and the next-fire heap.
type Scheduler struct {
	agentID string
	path    string
	run     RunFunc
	aud     *audit.Publisher
	log     *slog.Logger

	mu   sync.Mutex
	st   state
	pq   fireHeap
	wake chan struct{}
}

// New returns a Scheduler backed by schedule.json in stateDir.
func New(agentID, stateDir string, run RunFunc, aud *audit.Publisher, log *slog.Logger) *Scheduler {
	return &Scheduler{
		agentID: agentID,
		path:    filepath.Join(stateDir, FileName),
		run:     run,
		aud:     aud,
		log:     log,
		st:      state{LastFired: map[string]int64{}},
		wake:    make(chan struct{}, 1),
	}
}

// Load reads the persisted schedule.
func (s *Scheduler) Load() error {
	st, err := loadState(s.path)
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
	return err
}

// Version returns the cached schedule_version, which the heartbeat reports.
func (s *Scheduler) Version() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.ScheduleVersion
}

// Entries returns a copy of the cached entries.
func (s *Scheduler) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.st.Entries))
	copy(out, s.st.Entries)
	return out
}

// Sync replaces the cached schedule and persists it. Entries seen for the
// first time are marked as fired now, so a newly added nightly job does not
// immediately "catch up" a run it was never supposed to do.
//
// A document with an unusable entry_id is rejected whole rather than in
// part. Every entry id ends up inside a job id, and from there inside three
// publish subjects, so one bad id would poison the fleet's connection the
// first time that entry fired: hours later, with nothing pointing back at
// the sync that caused it.
func (s *Scheduler) Sync(doc Document) (int, error) {
	for _, e := range doc.Entries {
		if err := wire.CheckIdentifier("entry_id", e.EntryID); err != nil {
			return s.Version(), err
		}
	}
	now := time.Now()
	s.mu.Lock()
	lastFired := map[string]int64{}
	for _, e := range doc.Entries {
		if prev, ok := s.st.LastFired[e.EntryID]; ok {
			lastFired[e.EntryID] = prev
		} else {
			lastFired[e.EntryID] = now.Unix()
		}
	}
	s.st = state{
		ScheduleVersion: doc.ScheduleVersion,
		Entries:         doc.Entries,
		LastFired:       lastFired,
	}
	version := s.st.ScheduleVersion
	st := s.st
	s.mu.Unlock()

	if err := saveState(s.path, st); err != nil {
		return version, err
	}
	s.signal()
	return version, nil
}

// Run drives the schedule until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.Load(); err != nil {
		s.log.Warn("load schedule", "path", s.path, "err", err)
	}
	s.catchUp(ctx, time.Now())
	s.rebuild(time.Now())

	for {
		var wait <-chan time.Time
		var timer *time.Timer
		if item, ok := s.peek(); ok {
			timer = time.NewTimer(max(time.Until(item.at), 0))
			wait = timer.C
		}
		select {
		case <-ctx.Done():
			stop(timer)
			return ctx.Err()
		case <-s.wake:
			stop(timer)
			s.rebuild(time.Now())
		case <-wait:
			s.fireDue(ctx, time.Now())
		}
	}
}

// fireDue pops and runs everything whose jittered time has arrived.
func (s *Scheduler) fireDue(ctx context.Context, now time.Time) {
	for {
		item, ok := s.peek()
		if !ok || item.at.After(now) {
			return
		}
		s.pop()
		entry, ok := s.entry(item.entryID)
		if !ok {
			continue // removed by a sync between scheduling and firing
		}
		s.dispatch(ctx, entry, item.base)
		s.push(entry, now)
	}
}

// dispatch records the fire, persists it, and hands the run to the caller.
func (s *Scheduler) dispatch(ctx context.Context, entry Entry, base time.Time) {
	jobID := JobID(entry.EntryID, base)
	s.markFired(entry.EntryID, base)
	s.log.Info("schedule fire", "entry_id", entry.EntryID, "job_id", jobID,
		"kind", entry.Kind, "scheduled_for", base.UTC())
	if s.run != nil {
		go s.run(ctx, jobID, entry, base)
	}
}

// catchUp handles fires missed while the agent was down: the most recent one
// runs if it is inside its grace window, everything older is audited away.
func (s *Scheduler) catchUp(ctx context.Context, now time.Time) {
	for _, e := range s.Entries() {
		if !e.Enabled {
			continue
		}
		sch, loc, err := s.schedule(e)
		if err != nil {
			s.log.Warn("bad schedule entry", "entry_id", e.EntryID, "cron", e.Cron, "err", err)
			continue
		}
		since, ok := s.lastFired(e.EntryID)
		if !ok {
			s.markFired(e.EntryID, now)
			continue
		}
		missedAt, count := lastMissed(sch, since.In(loc), now.In(loc), maxScan)
		switch decideMisfire(missedAt, now, e.MisfireGraceS) {
		case misfireRun:
			// Spread the catch-up burst: after a fleet-wide outage every
			// agent would otherwise run the same job in the same second.
			at := now.Add(Jitter(s.agentID, e.EntryID, MisfireJitterS))
			s.mu.Lock()
			heap.Push(&s.pq, fireItem{entryID: e.EntryID, base: missedAt, at: at})
			s.mu.Unlock()
			s.log.Info("schedule misfire caught up", "entry_id", e.EntryID,
				"missed", count, "scheduled_for", missedAt.UTC(), "run_at", at.UTC())
		case misfireSkip:
			s.markFired(e.EntryID, missedAt)
			s.aud.Emit(audit.SchedMisfireSkip, map[string]any{
				"entry_id":        e.EntryID,
				"kind":            e.Kind,
				"missed_count":    count,
				"scheduled_for":   missedAt.UTC(),
				"misfire_grace_s": e.MisfireGraceS,
				"late_by_s":       int64(now.Sub(missedAt).Seconds()),
			})
			s.log.Warn("schedule misfire skipped", "entry_id", e.EntryID,
				"missed", count, "scheduled_for", missedAt.UTC())
		}
	}
	_ = ctx
}

// rebuild recomputes the whole heap, keeping any pending catch-up items.
func (s *Scheduler) rebuild(now time.Time) {
	s.mu.Lock()
	carried := fireHeap{}
	for _, it := range s.pq {
		if it.base.Before(now) {
			carried = append(carried, it) // a catch-up run still owed
		}
	}
	entries := make([]Entry, len(s.st.Entries))
	copy(entries, s.st.Entries)
	s.mu.Unlock()

	next := fireHeap{}
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		it, ok := s.itemFor(e, now)
		if !ok {
			continue
		}
		next = append(next, it)
	}
	next = append(next, carried...)

	s.mu.Lock()
	s.pq = next
	heap.Init(&s.pq)
	s.mu.Unlock()
}

// itemFor computes the next fire for an entry after now.
func (s *Scheduler) itemFor(e Entry, now time.Time) (fireItem, bool) {
	sch, loc, err := s.schedule(e)
	if err != nil {
		s.log.Warn("bad schedule entry", "entry_id", e.EntryID, "cron", e.Cron, "err", err)
		return fireItem{}, false
	}
	base := sch.Next(now.In(loc))
	if base.IsZero() {
		return fireItem{}, false
	}
	return fireItem{
		entryID: e.EntryID,
		base:    base,
		at:      base.Add(Jitter(s.agentID, e.EntryID, e.JitterS)),
	}, true
}

func (s *Scheduler) push(e Entry, now time.Time) {
	it, ok := s.itemFor(e, now)
	if !ok {
		return
	}
	s.mu.Lock()
	heap.Push(&s.pq, it)
	s.mu.Unlock()
}

func (s *Scheduler) peek() (fireItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pq.peek()
}

func (s *Scheduler) pop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pq.Len() > 0 {
		heap.Pop(&s.pq)
	}
}

func (s *Scheduler) entry(entryID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.st.Entries {
		if e.EntryID == entryID {
			return e, e.Enabled
		}
	}
	return Entry{}, false
}

func (s *Scheduler) lastFired(entryID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.st.LastFired[entryID]
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(ts, 0), true
}

func (s *Scheduler) markFired(entryID string, at time.Time) {
	s.mu.Lock()
	s.st.LastFired[entryID] = at.Unix()
	st := s.st
	s.mu.Unlock()
	if err := saveState(s.path, st); err != nil {
		s.log.Warn("persist schedule state", "path", s.path, "err", err)
	}
}

func (s *Scheduler) schedule(e Entry) (cron.Schedule, *time.Location, error) {
	loc := time.UTC
	if e.TZ != "" {
		l, err := time.LoadLocation(e.TZ)
		if err != nil {
			return nil, nil, fmt.Errorf("timezone %q: %w", e.TZ, err)
		}
		loc = l
	}
	sch, err := parser.Parse(e.Cron)
	if err != nil {
		return nil, nil, fmt.Errorf("cron %q: %w", e.Cron, err)
	}
	return sch, loc, nil
}

// signal nudges the run loop without blocking a caller.
func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// JobID is the idempotent id for a scheduled run.
func JobID(entryID string, base time.Time) string {
	return fmt.Sprintf("sched:%s:%d", entryID, base.Unix())
}

func stop(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}
