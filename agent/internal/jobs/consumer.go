package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openrmm/agent/internal/scripts"
	"github.com/openrmm/agent/internal/wire"
)

const (
	// StreamJobs is created and owned by the server.
	StreamJobs = "JOBS"

	// ackWait is how long the server waits for our ack before redelivering.
	// We ack as soon as a job starts, so this only covers the decode path.
	ackWait = 30 * time.Second

	// maxAckPending bounds how many jobs the server may push at us at once.
	maxAckPending = 16
)

// consume binds (or creates) this agent's durable pull consumer and runs it
// until ctx is cancelled.
func (m *Module) consume(ctx context.Context) error {
	js, err := jetstream.New(m.NC)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	cons, err := m.bindConsumer(ctx, js)
	if err != nil {
		return fmt.Errorf("bind job consumer: %w", err)
	}
	cc, err := cons.Consume(m.handleJob, jetstream.ConsumeErrHandler(
		func(_ jetstream.ConsumeContext, err error) {
			// Pull errors are transient (server restart, no responders while
			// the stream is being recreated); the library keeps retrying.
			m.Log.Warn("job consumer pull error", "err", err)
		}))
	if err != nil {
		return fmt.Errorf("consume jobs: %w", err)
	}
	defer cc.Stop()
	m.Log.Info("job consumer ready", "stream", StreamJobs,
		"durable", m.durableName(), "subject", wire.JobsQueue(m.AgentID))

	<-ctx.Done()
	return ctx.Err()
}

// durableName is the per-agent consumer name the server also uses when it
// wants to inspect the queue depth.
func (m *Module) durableName() string { return "agent-" + m.AgentID }

// bindConsumer prefers an existing durable so a server-side config (max
// deliver, backoff) is not clobbered by the agent on every restart.
func (m *Module) bindConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	name := m.durableName()
	cons, err := js.Consumer(ctx, StreamJobs, name)
	if err == nil {
		return cons, nil
	}
	if !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, err
	}
	return js.CreateConsumer(ctx, StreamJobs, jetstream.ConsumerConfig{
		Name:          name,
		Durable:       name,
		Description:   "openrmm agent " + m.AgentID,
		FilterSubject: wire.JobsQueue(m.AgentID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxAckPending: maxAckPending,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
}

// handleJob decodes one job and acks it as soon as execution STARTS. Acking
// on completion would redeliver every script that outlives ack_wait, which
// for a patch install means running it twice.
func (m *Module) handleJob(msg jetstream.Msg) {
	spec, err := decodeJob(msg.Data())
	if err != nil {
		m.Log.Warn("undecodable job, discarding", "err", err)
		if terr := msg.Term(); terr != nil {
			m.Log.Warn("job term", "err", terr)
		}
		return
	}
	if spec.JobID == "" {
		m.Log.Warn("job without job_id, discarding", "kind", spec.Kind)
		if terr := msg.Term(); terr != nil {
			m.Log.Warn("job term", "err", terr)
		}
		return
	}

	ctx := m.jobContext()
	m.Log.Info("job accepted", "job_id", spec.JobID, "kind", spec.Kind,
		"requested_by", spec.RequestedBy)
	go m.execute(ctx, spec)

	if err := msg.Ack(); err != nil {
		m.Log.Warn("job ack", "job_id", spec.JobID, "err", err)
	}
}

// jobContext is the lifetime of a dispatched job. It is deliberately not the
// consumer context: a job keeps running (and reporting) while the agent
// shuts down its subscriptions, and its own timeout ends it.
func (m *Module) jobContext() context.Context { return context.Background() }

// decodeJob accepts both an envelope-wrapped job and a bare job object.
func decodeJob(raw []byte) (scripts.JobSpec, error) {
	var spec scripts.JobSpec
	if err := json.Unmarshal(commandData(raw), &spec); err != nil {
		return spec, err
	}
	return spec, nil
}

// commandData unwraps a wire envelope, falling back to the raw body. The
// contract says every JSON message is an envelope; being liberal here means
// a hand-rolled `nats req` during debugging also works.
func commandData(raw []byte) json.RawMessage {
	var env wire.Envelope
	if err := json.Unmarshal(raw, &env); err == nil && env.V > 0 && len(env.Data) > 0 {
		return env.Data
	}
	return raw
}
