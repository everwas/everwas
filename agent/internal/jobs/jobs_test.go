package jobs

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/rsp2k/openrmm/agent/internal/sched"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
)

func testModule(t *testing.T) *Module {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Module{
		AgentID: "agent-1",
		Version: "test",
		Log:     log,
		Scripts: scripts.NewRunner(nil, "agent-1", t.TempDir(), nil, log),
		Sched:   sched.New("agent-1", t.TempDir(), nil, nil, log),
	}
}

func TestCommandDataUnwrapsEnvelope(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "envelope",
			raw:  `{"v":1,"type":"job","agent_id":"a","msg_id":"m","ts":"2026-08-15T12:00:00Z","data":{"job_id":"j1"}}`,
			want: `{"job_id":"j1"}`,
		},
		{
			name: "bare payload passes through",
			raw:  `{"job_id":"j1","kind":"script.run"}`,
			want: `{"job_id":"j1","kind":"script.run"}`,
		},
		{
			name: "envelope without data falls back to the body",
			raw:  `{"v":1,"type":"job"}`,
			want: `{"v":1,"type":"job"}`,
		},
		{
			name: "a payload with its own v field is not mistaken for an envelope",
			raw:  `{"v":1,"job_id":"j1"}`,
			want: `{"v":1,"job_id":"j1"}`,
		},
		{
			name: "garbage passes through for the caller to reject",
			raw:  `not json`,
			want: `not json`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(commandData([]byte(tt.raw)))
			if got != tt.want {
				t.Errorf("commandData = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDecodeJob(t *testing.T) {
	env := `{"v":1,"type":"job","agent_id":"a","msg_id":"m","data":
	  {"job_id":"j1","kind":"script.run","shell":"bash","body":"echo hi",
	   "timeout_s":60,"env":{"A":"B"},"requested_by":"ryan@example.com"}}`
	spec, err := decodeJob([]byte(env))
	if err != nil {
		t.Fatalf("decodeJob: %v", err)
	}
	want := scripts.JobSpec{
		JobID: "j1", Kind: "script.run", Shell: "bash", Body: "echo hi",
		TimeoutS: 60, Env: map[string]string{"A": "B"}, RequestedBy: "ryan@example.com",
	}
	if spec.JobID != want.JobID || spec.Kind != want.Kind || spec.Shell != want.Shell ||
		spec.Body != want.Body || spec.TimeoutS != want.TimeoutS ||
		spec.RequestedBy != want.RequestedBy || spec.Env["A"] != "B" {
		t.Errorf("decodeJob = %+v, want %+v", spec, want)
	}

	if _, err := decodeJob([]byte(`{"job_id": [1,2]}`)); err == nil {
		t.Error("want an error for a job with the wrong field types")
	}
}

func TestCmdSchedSync(t *testing.T) {
	m := testModule(t)
	doc := sched.Document{
		ScheduleVersion: 12,
		Entries: []sched.Entry{{
			EntryID: "nightly", Cron: "0 3 * * *", TZ: "UTC",
			Kind: "script.run", JitterS: 900, MisfireGraceS: 3600, Enabled: true,
		}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := m.cmdSchedSync(raw)
	if !got.Accepted || got.ScheduleVersion != 12 {
		t.Fatalf("sched.sync reply = %+v, want accepted at version 12", got)
	}
	if m.Sched.Version() != 12 {
		t.Errorf("scheduler version = %d, want 12", m.Sched.Version())
	}
	if entries := m.Sched.Entries(); len(entries) != 1 || entries[0].JitterS != 900 {
		t.Errorf("entries not applied: %+v", entries)
	}

	if bad := m.cmdSchedSync([]byte(`{"schedule_version": "twelve"}`)); bad.Accepted {
		t.Error("a malformed schedule should be refused")
	} else if !strings.Contains(bad.Error, "sched.sync") {
		t.Errorf("error %q should name the command", bad.Error)
	}
}

func TestCmdJobCancelUnknownJob(t *testing.T) {
	m := testModule(t)
	got := m.cmdJobCancel([]byte(`{"job_id":"nope","requested_by":"ryan"}`))
	if got.Accepted {
		t.Error("cancelling an unknown job should not be accepted")
	}
	if got.Cancelled == nil || *got.Cancelled {
		t.Errorf("cancelled = %v, want false", got.Cancelled)
	}
	if got.JobID != "nope" {
		t.Errorf("job_id = %q, want it echoed back", got.JobID)
	}
}

func TestCmdRepliesAreBareJSON(t *testing.T) {
	raw, err := json.Marshal(reply{Accepted: true, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := got["data"]; wrapped {
		t.Error("command replies must not be envelope-wrapped")
	}
	if got["accepted"] != true || got["session_id"] != "s1" {
		t.Errorf("reply = %v", got)
	}
	// Omitted fields must not appear as zero values the server has to filter.
	for _, k := range []string{"error", "job_id", "schedule_version", "cancelled"} {
		if _, ok := got[k]; ok {
			t.Errorf("empty field %q was serialised", k)
		}
	}
}

func TestDurableName(t *testing.T) {
	m := testModule(t)
	if got := m.durableName(); got != "agent-agent-1" {
		t.Errorf("durableName = %q", got)
	}
	if strings.ContainsAny(m.durableName(), ".*> ") {
		t.Error("durable name contains characters NATS forbids in consumer names")
	}
}
