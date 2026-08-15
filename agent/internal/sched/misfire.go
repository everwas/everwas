package sched

import (
	"time"

	"github.com/robfig/cron/v3"
)

// maxScan bounds the catch-up walk. A per-minute entry on an agent that was
// off for a week is ~10k steps; past that we only care that something was
// missed, not how many times.
const maxScan = 20000

type misfireAction int

const (
	misfireNone misfireAction = iota // nothing was missed
	misfireRun                       // missed recently enough to catch up
	misfireSkip                      // too old; record and move on
)

func (a misfireAction) String() string {
	switch a {
	case misfireRun:
		return "run"
	case misfireSkip:
		return "skip"
	default:
		return "none"
	}
}

// decideMisfire is the whole misfire policy: catch up once if the missed
// fire is inside the grace window, otherwise skip it loudly. A grace of 0
// means "this job is worthless late" — never catch up.
func decideMisfire(missedAt, now time.Time, graceS int) misfireAction {
	if missedAt.IsZero() {
		return misfireNone
	}
	if graceS <= 0 {
		return misfireSkip
	}
	if now.Sub(missedAt) <= time.Duration(graceS)*time.Second {
		return misfireRun
	}
	return misfireSkip
}

// lastMissed walks a schedule from since to now and returns the most recent
// fire time in that window plus how many fires were missed.
func lastMissed(s cron.Schedule, since, now time.Time, scanLimit int) (time.Time, int) {
	var last time.Time
	count := 0
	t := since
	for i := 0; i < scanLimit; i++ {
		next := s.Next(t)
		if next.IsZero() || !next.After(t) || next.After(now) {
			break
		}
		last, count, t = next, count+1, next
	}
	return last, count
}
