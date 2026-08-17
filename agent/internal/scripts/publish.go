package scripts

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/rsp2k/openrmm/agent/internal/wire"
)

const (
	// resultAttempts and resultBackoff bound how hard we try to get a
	// terminal result acknowledged.
	resultAttempts = 3
	resultBackoff  = 250 * time.Millisecond

	// resultAckWait is how long one publish waits for the server's ack.
	resultAckWait = 5 * time.Second
)

// resultPublisher is the JetStream publish path. jetstream.JetStream
// satisfies it; tests provide a fake.
type resultPublisher interface {
	PublishMsg(context.Context, *nats.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

// progressMsg is the data payload of a job_progress envelope.
type progressMsg struct {
	Seq   int    `json:"seq"`
	Pct   int    `json:"pct"`
	Phase string `json:"phase"`
	Note  string `json:"note"`
}

// Progress returns a ProgressFunc that publishes on
// agents.{id}.jobs.{job_id}.progress with a per-job sequence number.
// Progress is core NATS: it is advisory, and a lost frame must not stall
// the job.
func (r *Runner) Progress(jobID string) ProgressFunc {
	var seq atomic.Int64
	return func(pct int, phase, note string) {
		msg := progressMsg{Seq: int(seq.Add(1)), Pct: pct, Phase: phase, Note: note}
		raw, _, err := r.envelope("job_progress", msg)
		if err != nil {
			r.warn("job progress envelope", "job_id", jobID, "err", err)
			return
		}
		if r.NC == nil {
			return
		}
		if err := r.NC.Publish(wire.JobProgress(r.AgentID, jobID), raw); err != nil {
			r.warn("job progress publish", "job_id", jobID, "err", err)
		}
	}
}

// publishChunk sends one output frame on the JOBOUT stream.
func (r *Runner) publishChunk(c Chunk) error {
	raw, msgID, err := r.envelope("job_output", c)
	if err != nil {
		return err
	}
	if r.NC == nil {
		return nil
	}
	return r.NC.PublishMsg(&nats.Msg{
		Subject: wire.JobOutput(r.AgentID, c.JobID),
		Data:    raw,
		Header:  nats.Header{"Nats-Msg-Id": []string{msgID}},
	})
}

// publishResult sends the terminal result on the RESULTS stream.
//
// This is the one message a job cannot afford to lose: without it the server
// shows the job running forever. A core publish is fire and forget, so the
// agent could not tell "the server has the result" from "it evaporated
// during a reconnect". The JetStream path waits for the ack and retries;
// the Nats-Msg-Id header makes the retry a dedup rather than a duplicate.
func (r *Runner) publishResult(jobID string, res Result) {
	raw, msgID, err := r.envelope("job_result", res)
	if err != nil {
		r.warn("job result envelope", "job_id", jobID, "err", err)
		return
	}
	msg := &nats.Msg{
		Subject: wire.JobResult(r.AgentID, jobID),
		Data:    raw,
		Header:  nats.Header{"Nats-Msg-Id": []string{msgID}},
	}
	js := r.resultStream()
	if js == nil {
		if r.NC == nil {
			return
		}
		// No JetStream context: a core publish is worse than an ack but far
		// better than dropping the only terminal message the job has.
		if err := r.NC.PublishMsg(msg); err != nil {
			r.warn("job result publish", "job_id", jobID, "err", err)
		}
		return
	}
	if err := publishAck(js, msg); err != nil {
		r.warn("job result was never acknowledged by the server",
			"job_id", jobID, "status", res.Status, "attempts", resultAttempts, "err", err)
	}
}

// publishAck publishes until the server acks or the attempts run out.
func publishAck(js resultPublisher, msg *nats.Msg) error {
	var err error
	for attempt := range resultAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * resultBackoff)
		}
		ctx, cancel := context.WithTimeout(context.Background(), resultAckWait)
		_, err = js.PublishMsg(ctx, msg)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// resultStream builds the JetStream publisher once, lazily: NewRunner is
// called before the connection has necessarily reached a server.
func (r *Runner) resultStream() resultPublisher {
	r.resultsOnce.Do(func() {
		if r.results != nil || r.NC == nil {
			return
		}
		js, err := jetstream.New(r.NC)
		if err != nil {
			r.warn("jetstream context for job results", "err", err)
			return
		}
		r.results = js
	})
	return r.results
}

// PublishResult publishes a terminal result for work the runner did not
// execute itself (inventory refresh, unsupported job kinds).
// PublishResult sends one terminal result for a job.
//
// Takes the JobSpec rather than a job id so EntryID travels with it. It used to
// take an id, and every caller except the happy path built a bare Result and
// dropped the entry id, which is the only thing that lets the server attribute
// a scheduled run: a nightly job that panicked or was interrupted left no
// record anywhere. Threading the spec is what makes forgetting impossible,
// rather than remembering to set a field at six call sites.
func (r *Runner) PublishResult(job JobSpec, res Result) {
	res.EntryID = job.EntryID
	if r.OnResult != nil {
		r.OnResult(job.JobID, res)
		return
	}
	r.publishResult(job.JobID, res)
}

// PublishStderr sends one stderr chunk and both EOF markers, so a job that
// never spawned a process still terminates its output stream cleanly.
func (r *Runner) PublishStderr(job JobSpec, text string) {
	jobID := job.JobID
	// Output is adopted by the same lookup as the result, so a chunk with no
	// entry id is dropped alongside it and the operator loses the reason as
	// well as the record.
	sink := newChunkSink(jobID, job.EntryID, r.chunkOut)
	if err := sink.write(StreamStderr, []byte(text)); err != nil {
		r.warn("job stderr publish", "job_id", jobID, "err", err)
	}
	for _, stream := range []string{StreamStdout, StreamStderr} {
		if err := sink.eof(stream); err != nil {
			r.warn("job output eof", "job_id", jobID, "err", err)
		}
	}
}

// envelope marshals data and returns the bytes plus the msg_id, so callers
// can mirror it into the Nats-Msg-Id header for JetStream dedup.
func (r *Runner) envelope(msgType string, data any) ([]byte, string, error) {
	env, err := wire.NewEnvelope(msgType, r.AgentID, wire.NewMsgID(), data)
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, "", err
	}
	return raw, env.MsgID, nil
}
