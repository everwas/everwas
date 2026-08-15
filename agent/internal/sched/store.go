package sched

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// FileName is the schedule cache inside the agent state dir.
const FileName = "schedule.json"

// Entry is one scheduled job as delivered by cmd.{id}.sched.sync.
type Entry struct {
	EntryID       string          `json:"entry_id"`
	Cron          string          `json:"cron"`
	TZ            string          `json:"tz"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	JitterS       int             `json:"jitter_s"`
	MisfireGraceS int             `json:"misfire_grace_s"`
	Enabled       bool            `json:"enabled"`
}

// Document is the sched.sync payload.
type Document struct {
	ScheduleVersion int     `json:"schedule_version"`
	Entries         []Entry `json:"entries"`
}

// state is the on-disk form: the document plus the fire bookkeeping that
// lets the agent decide what it missed while it was down.
type state struct {
	ScheduleVersion int              `json:"schedule_version"`
	Entries         []Entry          `json:"entries"`
	LastFired       map[string]int64 `json:"last_fired"`
}

// loadState reads the cache. A missing file is an empty schedule, not an
// error: a fresh agent simply has not been told anything yet.
func loadState(path string) (state, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{LastFired: map[string]int64{}}, nil
	}
	if err != nil {
		return state{LastFired: map[string]int64{}}, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{LastFired: map[string]int64{}}, err
	}
	if st.LastFired == nil {
		st.LastFired = map[string]int64{}
	}
	return st, nil
}

// saveState writes the cache 0600 via rename, so a crash mid-write cannot
// leave a half-parsed schedule behind.
func saveState(path string, st state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
