package audit

import (
	"encoding/json"
	"testing"
	"time"
)

// TestEmitIsNilSafe: audit must never be the reason an operation fails, so
// both a nil Publisher and one without a connection are no-ops.
func TestEmitIsNilSafe(t *testing.T) {
	var p *Publisher
	p.Emit(ShellOpened, map[string]any{"session_id": "s1"})
	New(nil, "agent-1", nil).Emit(ShellClosed, nil)
}

func TestRecordShape(t *testing.T) {
	raw, err := json.Marshal(record{
		Event:  ScriptExecuted,
		At:     time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Detail: map[string]any{"exit_code": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "script.executed" {
		t.Errorf("event = %v", got["event"])
	}
	if got["at"] != "2026-08-15T12:00:00Z" {
		t.Errorf("at = %v, want RFC 3339 UTC", got["at"])
	}
	detail, ok := got["detail"].(map[string]any)
	if !ok || detail["exit_code"] != float64(0) {
		t.Errorf("detail = %v", got["detail"])
	}
}

func TestEventNames(t *testing.T) {
	want := map[string]string{
		ShellOpened:      "shell.opened",
		ShellClosed:      "shell.closed",
		ScriptExecuted:   "script.executed",
		JobCancelled:     "job.cancelled",
		SchedMisfireSkip: "sched.misfire_skipped",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("event constant = %q, want %q", got, expect)
		}
	}
}
