package scripts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fakeResults records publishes and fails the first failures of them.
type fakeResults struct {
	mu       sync.Mutex
	failures int
	msgs     []*nats.Msg
}

func (f *fakeResults) PublishMsg(_ context.Context, msg *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msg)
	if len(f.msgs) <= f.failures {
		return nil, errors.New("no responders")
	}
	return &jetstream.PubAck{Stream: "RESULTS", Sequence: uint64(len(f.msgs))}, nil
}

func (f *fakeResults) calls() []*nats.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nats.Msg(nil), f.msgs...)
}

func newResultRunner(t *testing.T, fake *fakeResults) *Runner {
	t.Helper()
	r := NewRunner(nil, "agent-1", t.TempDir(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.results = fake
	return r
}

// TestPublishResultWaitsForAnAck is the regression for terminal results
// going out as fire-and-forget core publishes. A result that evaporates
// during a reconnect leaves the server showing the job running forever, and
// the agent could not tell the difference. The result must go through the
// JetStream path, which acks.
func TestPublishResultWaitsForAnAck(t *testing.T) {
	fake := &fakeResults{}
	r := newResultRunner(t, fake)
	r.publishResult("j-1", Result{Status: StatusSucceeded})

	msgs := fake.calls()
	if len(msgs) != 1 {
		t.Fatalf("%d acknowledged publishes, want 1", len(msgs))
	}
	if got := msgs[0].Subject; got != "agents.agent-1.jobs.j-1.result" {
		t.Errorf("subject = %q", got)
	}
	if id := msgs[0].Header.Get("Nats-Msg-Id"); id == "" {
		t.Error("no Nats-Msg-Id, so a retry would duplicate the result")
	}
}

// TestPublishResultRetriesUntilAcked: a blip must not lose the result, and
// the retry must be bounded so a job goroutine cannot wedge on shutdown.
func TestPublishResultRetriesUntilAcked(t *testing.T) {
	fake := &fakeResults{failures: resultAttempts - 1}
	r := newResultRunner(t, fake)
	r.publishResult("j-2", Result{Status: StatusFailed, ExitCode: 3})

	msgs := fake.calls()
	if len(msgs) != resultAttempts {
		t.Fatalf("%d attempts, want %d", len(msgs), resultAttempts)
	}
	first := msgs[0].Header.Get("Nats-Msg-Id")
	for i, m := range msgs {
		if got := m.Header.Get("Nats-Msg-Id"); got != first {
			t.Errorf("attempt %d used msg id %q, want %q: the stream cannot dedup it", i, got, first)
		}
	}
}

func TestPublishResultGivesUpAfterTheAttempts(t *testing.T) {
	fake := &fakeResults{failures: 100}
	r := newResultRunner(t, fake)
	r.publishResult("j-3", Result{Status: StatusTimeout})

	if got := len(fake.calls()); got != resultAttempts {
		t.Errorf("%d attempts, want the retry bounded at %d", got, resultAttempts)
	}
}
