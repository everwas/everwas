// Package jobs is the agent's command surface: a durable JetStream pull
// consumer for work that must survive downtime, and core NATS request/reply
// for interactive commands that only make sense right now.
package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/sched"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/shell"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

// consumerRetry is how long we wait before rebinding the durable consumer
// after a JetStream failure.
const consumerRetry = 10 * time.Second

// Module wires the job consumer and command handler to the other modules.
type Module struct {
	NC      *nats.Conn
	AgentID string
	Version string
	Log     *slog.Logger

	Shell   *shell.Module
	Scripts *scripts.Runner
	Sched   *sched.Scheduler
	Audit   *audit.Publisher

	// Patch handles patch.scan and patch.install.
	Patch PatchDeps

	// RefreshInventory runs an out-of-band inventory snapshot.
	RefreshInventory func(context.Context) error
}

// Run subscribes to commands and consumes the job queue until ctx is done.
func (m *Module) Run(ctx context.Context) error {
	sub, err := m.NC.Subscribe(wire.CmdWildcard(m.AgentID), m.handleCommand)
	if err != nil {
		return err
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			m.Log.Debug("cmd unsubscribe", "err", err)
		}
	}()
	m.Log.Info("command handler ready", "subject", wire.CmdWildcard(m.AgentID))

	// The job consumer is retried in place rather than by failing the whole
	// task: JetStream may lag behind (the server creates the JOBS stream),
	// and interactive commands must keep working while it does.
	for {
		err := m.consume(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.Log.Warn("job consumer stopped, retrying", "err", err,
			"retry_in", consumerRetry.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(consumerRetry):
		}
	}
}

// execute dispatches one decoded job. It runs on its own goroutine: the
// JetStream message was already acked by the time we get here.
func (m *Module) execute(ctx context.Context, spec scripts.JobSpec) {
	progress := m.Scripts.Progress(spec.JobID)
	switch spec.Kind {
	case scripts.KindScriptRun, "":
		m.Scripts.Run(ctx, spec, progress)
	case scripts.KindInventoryRefresh:
		m.runInventoryRefresh(ctx, spec, progress)
	case scripts.KindPatchScan, scripts.KindPatchInstall:
		m.Patch.Execute(ctx, spec, progress)
	default:
		m.unsupportedJob(spec, progress)
	}
}

func (m *Module) runInventoryRefresh(ctx context.Context, spec scripts.JobSpec, progress scripts.ProgressFunc) {
	started := time.Now()
	progress(0, scripts.PhaseStarted, spec.Kind)
	res := scripts.Result{Status: scripts.StatusSucceeded}
	if m.RefreshInventory == nil {
		res = scripts.Result{Status: scripts.StatusFailed, ExitCode: -1}
	} else if err := m.RefreshInventory(ctx); err != nil {
		m.Log.Warn("inventory refresh job failed", "job_id", spec.JobID, "err", err)
		res = scripts.Result{Status: scripts.StatusFailed, ExitCode: -1}
	}
	res.DurationMS = time.Since(started).Milliseconds()
	progress(100, scripts.PhaseFinished, res.Status)
	m.Scripts.PublishResult(spec.JobID, res)
}

// unsupportedJob reports a kind this build cannot run (patch.* lands in M5)
// without leaving the server waiting for a result that never comes.
func (m *Module) unsupportedJob(spec scripts.JobSpec, progress scripts.ProgressFunc) {
	note := "job kind " + spec.Kind + " is not supported by agent " + m.Version
	m.Log.Warn("unsupported job kind", "job_id", spec.JobID, "kind", spec.Kind)
	progress(0, scripts.PhaseStarted, spec.Kind)
	m.Scripts.PublishStderr(spec.JobID, "openrmm-agent: "+note+"\n")
	progress(100, scripts.PhaseFinished, scripts.StatusFailed)
	m.Scripts.PublishResult(spec.JobID, scripts.Result{
		Status:   scripts.StatusFailed,
		ExitCode: -1,
	})
	m.Audit.Emit(audit.CommandUnsupported, map[string]any{
		"job_id": spec.JobID,
		"kind":   spec.Kind,
	})
}

// RunScheduled is the sched.RunFunc: a schedule entry's payload is a job
// spec, with the job id fixed by the schedule for server-side idempotency.
func (m *Module) RunScheduled(ctx context.Context, jobID string, entry sched.Entry, fireAt time.Time) {
	spec := scripts.JobSpec{Kind: entry.Kind}
	if len(entry.Payload) > 0 {
		if err := json.Unmarshal(entry.Payload, &spec); err != nil {
			m.Log.Warn("bad schedule payload", "entry_id", entry.EntryID, "err", err)
			return
		}
	}
	spec.JobID = jobID
	if spec.Kind == "" {
		spec.Kind = entry.Kind
	}
	if spec.RequestedBy == "" {
		spec.RequestedBy = "schedule:" + entry.EntryID
	}
	m.Log.Info("running scheduled job", "job_id", jobID, "entry_id", entry.EntryID,
		"kind", spec.Kind, "scheduled_for", fireAt.UTC())
	m.execute(ctx, spec)
}
