package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

const (
	// StreamJobs is created and owned by the server.
	StreamJobs = "JOBS"

	// ackWait is how long the server waits for our ack before redelivering.
	// We ack as soon as a job starts, so this only covers the decode path.
	ackWait = 30 * time.Second

	// maxAckPending bounds how many jobs the server may push at us at once.
	maxAckPending = 16

	// maxDeliver bounds redelivery. Unset means unlimited, so a job that
	// kills the agent on the way in comes back forever. Three attempts is
	// enough to survive an ack lost to a reconnect and few enough that a
	// poisonous job ends up on the dead letter path instead of in a loop.
	maxDeliver = 3
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
			// Most pull errors are transient and the library retries them.
			// Some are not: on a deleted consumer or a bad request it calls
			// this handler once and then stops the subscription for good.
			// That is why the wait below watches Closed as well.
			m.Log.Warn("job consumer pull error", "err", err)
		}))
	if err != nil {
		return fmt.Errorf("consume jobs: %w", err)
	}
	defer cc.Stop()
	m.Log.Info("job consumer ready", "stream", StreamJobs,
		"durable", m.durableName(), "subject", wire.JobsQueue(m.AgentID))

	return waitConsumer(ctx, cc.Closed())
}

// waitConsumer blocks until the agent is stopping or the consumer stops
// itself, and returns an error in the second case so the caller rebinds.
//
// Parking on ctx.Done() alone was the defect: a terminal pull error (the
// JOBS stream recreated by a restore or a retention change, which deletes
// the consumer) makes the library stop the subscription permanently. The
// agent went on heartbeating healthy while executing nothing, forever.
func waitConsumer(ctx context.Context, closed <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return errors.New("job consumer was stopped by the client after a terminal pull error")
	}
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
		MaxDeliver:    maxDeliver,
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
	// job_id goes straight into the progress, output and result subjects. An
	// id carrying a wildcard would make every publish illegal and take the
	// connection down with it, so an unusable id is terminated here rather
	// than executed. Term, not Nak: redelivering it would fail identically.
	if err := wire.CheckIdentifier("job_id", spec.JobID); err != nil {
		m.Log.Warn("job with unusable job_id, discarding", "kind", spec.Kind, "err", err)
		if terr := msg.Term(); terr != nil {
			m.Log.Warn("job term", "err", terr)
		}
		return
	}

	// Take a worker slot and register the job BEFORE acking. Blocking here
	// is deliberate: the library delivers to this callback one message at a
	// time, so an agent with every slot busy stops acking and MaxAckPending
	// becomes real backpressure instead of a number.
	ctx, release, err := m.reserve(spec)
	switch {
	case errors.Is(err, errJobDone):
		// Ran already and finished. Acking is the whole point: leaving it
		// unacked means JetStream keeps offering it until MaxDeliver, and
		// every offer is another chance to run it a second time.
		m.Log.Warn("redelivery of a completed job, dropping",
			"job_id", spec.JobID, "kind", spec.Kind)
		if aerr := msg.Ack(); aerr != nil {
			m.Log.Warn("job ack", "job_id", spec.JobID, "err", aerr)
		}
		return
	case errors.Is(err, errJobRunning):
		// A redelivery of a job we are already running, which happens
		// whenever an ack is lost to a blip longer than ack_wait. A second
		// execution would share work/{job_id}/ with the first: one truncates
		// the other's script, one removes the directory under the other, and
		// for patch.install it is two concurrent package manager runs.
		m.Log.Warn("duplicate delivery of a running job, dropping",
			"job_id", spec.JobID, "kind", spec.Kind)
		if aerr := msg.Ack(); aerr != nil {
			m.Log.Warn("job ack", "job_id", spec.JobID, "err", aerr)
		}
		return
	case err != nil:
		// Shutting down. Leave it on the stream for the next process rather
		// than starting work we cannot finish.
		m.Log.Warn("job not started", "job_id", spec.JobID, "err", err)
		if nerr := msg.Nak(); nerr != nil {
			m.Log.Warn("job nak", "job_id", spec.JobID, "err", nerr)
		}
		return
	}

	m.Log.Info("job accepted", "job_id", spec.JobID, "kind", spec.Kind,
		"requested_by", spec.RequestedBy)
	go func() {
		defer release()
		// Marked done BEFORE the slot is released, so a redelivery cannot slip
		// between the two and find the id neither running nor finished.
		defer m.markDone(spec.JobID)
		m.runJob(ctx, spec)
	}()

	if err := msg.Ack(); err != nil {
		m.Log.Warn("job ack", "job_id", spec.JobID, "err", err)
	}
}

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
