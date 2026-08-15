package sched

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func TestDecideMisfire(t *testing.T) {
	now := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		missedAt time.Time
		graceS   int
		want     misfireAction
	}{
		{"nothing missed", time.Time{}, 3600, misfireNone},
		{"nothing missed, no grace", time.Time{}, 0, misfireNone},
		{"missed a minute ago, hour of grace", now.Add(-time.Minute), 3600, misfireRun},
		{"missed exactly at the grace boundary", now.Add(-time.Hour), 3600, misfireRun},
		{"one second past the grace boundary", now.Add(-time.Hour - time.Second), 3600, misfireSkip},
		{"zero grace never catches up", now.Add(-time.Second), 0, misfireSkip},
		{"negative grace never catches up", now.Add(-time.Second), -1, misfireSkip},
		{"missed days ago", now.AddDate(0, 0, -3), 3600, misfireSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideMisfire(tt.missedAt, now, tt.graceS); got != tt.want {
				t.Errorf("decideMisfire = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLastMissed(t *testing.T) {
	mustParse := func(spec string) cron.Schedule {
		t.Helper()
		s, err := parser.Parse(spec)
		if err != nil {
			t.Fatalf("parse %q: %v", spec, err)
		}
		return s
	}
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		cron      string
		since     time.Time
		now       time.Time
		wantLast  time.Time
		wantCount int
	}{
		{
			name: "no fires in the window", cron: "0 3 * * *",
			since: base, now: base.Add(time.Hour),
			wantLast: time.Time{}, wantCount: 0,
		},
		{
			name: "one missed daily fire", cron: "0 3 * * *",
			since: base, now: base.Add(4 * time.Hour),
			wantLast: base.Add(3 * time.Hour), wantCount: 1,
		},
		{
			name: "three days down keeps only the newest", cron: "0 3 * * *",
			since: base, now: base.AddDate(0, 0, 3),
			wantLast: base.AddDate(0, 0, 2).Add(3 * time.Hour), wantCount: 3,
		},
		{
			name: "hourly over half a day", cron: "0 * * * *",
			since: base, now: base.Add(12*time.Hour + 30*time.Minute),
			wantLast: base.Add(12 * time.Hour), wantCount: 12,
		},
		{
			name: "a fire exactly at now counts as missed", cron: "0 * * * *",
			since: base, now: base.Add(time.Hour),
			wantLast: base.Add(time.Hour), wantCount: 1,
		},
		{
			name: "descriptor schedules parse", cron: "@daily",
			since: base, now: base.AddDate(0, 0, 1).Add(time.Hour),
			wantLast: base.AddDate(0, 0, 1), wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := lastMissed(mustParse(tt.cron), tt.since, tt.now, maxScan)
			if !got.Equal(tt.wantLast) {
				t.Errorf("last = %s, want %s", got, tt.wantLast)
			}
			if count != tt.wantCount {
				t.Errorf("count = %d, want %d", count, tt.wantCount)
			}
		})
	}
}

// TestLastMissedRespectsScanLimit keeps a year-long outage with a per-minute
// entry from spinning: the walk stops at the limit and the caller still gets
// a usable "something was missed" answer.
func TestLastMissedRespectsScanLimit(t *testing.T) {
	s, err := parser.Parse("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	last, count := lastMissed(s, base, base.AddDate(1, 0, 0), 50)
	if count != 50 {
		t.Errorf("count = %d, want the scan limit 50", count)
	}
	if !last.Equal(base.Add(50 * time.Minute)) {
		t.Errorf("last = %s, want %s", last, base.Add(50*time.Minute))
	}
}
