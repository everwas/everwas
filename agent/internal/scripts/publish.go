package scripts

import (
	"encoding/json"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/wire"
)

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
func (r *Runner) publishResult(jobID string, res Result) {
	raw, msgID, err := r.envelope("job_result", res)
	if err != nil {
		r.warn("job result envelope", "job_id", jobID, "err", err)
		return
	}
	if r.NC == nil {
		return
	}
	err = r.NC.PublishMsg(&nats.Msg{
		Subject: wire.JobResult(r.AgentID, jobID),
		Data:    raw,
		Header:  nats.Header{"Nats-Msg-Id": []string{msgID}},
	})
	if err != nil {
		r.warn("job result publish", "job_id", jobID, "err", err)
	}
}

// PublishResult publishes a terminal result for work the runner did not
// execute itself (inventory refresh, unsupported job kinds).
func (r *Runner) PublishResult(jobID string, res Result) {
	r.publishResult(jobID, res)
}

// PublishStderr sends one stderr chunk and both EOF markers, so a job that
// never spawned a process still terminates its output stream cleanly.
func (r *Runner) PublishStderr(jobID, text string) {
	sink := newChunkSink(jobID, r.chunkOut)
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
