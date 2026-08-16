package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fakeMsg is the smallest jetstream.Msg that handleJob can be driven with.
// It records which disposition was chosen, which is the whole point: a job
// the agent cannot safely run must be terminated, not acked and executed.
type fakeMsg struct {
	data     []byte
	acked    bool
	termed   bool
	naked    bool
	progress bool
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return &jetstream.MsgMetadata{}, nil }
func (m *fakeMsg) Data() []byte                              { return m.data }
func (m *fakeMsg) Headers() nats.Header                      { return nats.Header{} }
func (m *fakeMsg) Subject() string                           { return "jobs.agent-1" }
func (m *fakeMsg) Reply() string                             { return "" }
func (m *fakeMsg) Ack() error                                { m.acked = true; return nil }
func (m *fakeMsg) DoubleAck(context.Context) error           { m.acked = true; return nil }
func (m *fakeMsg) Nak() error                                { m.naked = true; return nil }
func (m *fakeMsg) NakWithDelay(time.Duration) error          { m.naked = true; return nil }
func (m *fakeMsg) InProgress() error                         { m.progress = true; return nil }
func (m *fakeMsg) Term() error                               { m.termed = true; return nil }
func (m *fakeMsg) TermWithReason(string) error               { m.termed = true; return nil }

// TestHandleJobRefusesUnusableJobIDs pins the intake boundary. job_id is
// interpolated into the progress, output and result subjects, so a wildcard
// in it makes every publish for that job illegal, and an unrecognised -ERR
// from the server closes the agent's connection permanently. The job must be
// terminated before anything is dispatched.
func TestHandleJobRefusesUnusableJobIDs(t *testing.T) {
	bad := []struct {
		name string
		body string
	}{
		{"full wildcard", `{"job_id":">","kind":"script.run"}`},
		{"token wildcard", `{"job_id":"*","kind":"script.run"}`},
		{"wildcard in a plausible id", `{"job_id":"job->1","kind":"script.run"}`},
		{"empty", `{"job_id":"","kind":"script.run"}`},
		{"missing", `{"kind":"script.run"}`},
		{"whitespace", `{"job_id":"a b","kind":"script.run"}`},
		{"dot dot", `{"job_id":"..","kind":"script.run"}`},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			m := testModule(t)
			msg := &fakeMsg{data: []byte(tt.body)}
			m.handleJob(msg)
			if !msg.termed {
				t.Errorf("job was not terminated; acked=%v naked=%v", msg.acked, msg.naked)
			}
			if msg.acked {
				t.Error("job was acked, which means it was dispatched")
			}
		})
	}
}

// TestHandleJobAcceptsAUUID keeps the gate from being a wall: the ids the
// server actually issues must still get through.
func TestHandleJobAcceptsAUUID(t *testing.T) {
	m := testModule(t)
	msg := &fakeMsg{data: []byte(`{"job_id":"01991111-2222-7333-8444-555566667777","kind":"nope.unsupported"}`)}
	m.handleJob(msg)
	if msg.termed {
		t.Error("a well-formed job id was terminated")
	}
	if !msg.acked {
		t.Error("a well-formed job was not acked")
	}
}
