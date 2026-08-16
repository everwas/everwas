// Package jobs is the agent's command surface: a durable JetStream pull
// consumer for work that must survive downtime, and core NATS request/reply
// for interactive commands that only make sense right now.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
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

// EventJobPanicked is emitted when a job dies of a panic. It belongs next to
// the other names in internal/audit; it lives here until that constant block
// is extended, same as the patch events.
const EventJobPanicked = "job.panicked"

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

	// The in-flight registry: one cancellation hook per running job, a
	// bounded set of worker slots, and the parent context every dispatched
	// job derives from. See inflight.go.
	mu       sync.Mutex
	inflight map[string]*jobHandle
	slots    chan struct{}
	base     context.Context
	stopJobs context.CancelFunc
	stopping bool
	wg       sync.WaitGroup

	// Shutdown timings; zero means the defaults. Tests shorten them.
	shutdownGrace time.Duration
	cancelGrace   time.Duration
}

// Run subscribes to commands and consumes the job queue until ctx is done,
// then brings every running job to a terminal state before returning.
func (m *Module) Run(ctx context.Context) error {
	m.startJobs()
	defer m.drainJobs()

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

// runJob is the only path that executes a job.
//
// Every dispatch goes through the recover: a job runs on its own goroutine,
// outside the supervisor's runRecovered, and it is where server-supplied
// data gets parsed. An unrecovered panic there takes the whole agent down,
// and because a panic can beat the ack, JetStream then redelivers the same
// job and crash-loops every agent that receives it.
func (m *Module) runJob(ctx context.Context, spec scripts.JobSpec) {
	defer func() {
		if r := recover(); r != nil {
			m.jobPanicked(spec, r, debug.Stack())
		}
	}()
	m.execute(ctx, spec)
}

// jobPanicked ends a panicked job cleanly: the operator gets the reason on
// the job's own output stream and a terminal result, so the job stops rather
// than hanging while the agent restarts.
func (m *Module) jobPanicked(spec scripts.JobSpec, cause any, stack []byte) {
	m.Log.Error("panic while running a job", "job_id", spec.JobID,
		"kind", spec.Kind, "panic", cause, "stack", string(stack))
	if m.Scripts != nil {
		m.Scripts.PublishStderr(spec.JobID,
			fmt.Sprintf("openrmm-agent: job failed with an internal error: %v\n", cause))
		m.Scripts.PublishResult(spec.JobID, scripts.Result{
			Status:   scripts.StatusFailed,
			ExitCode: -1,
		})
	}
	m.Audit.Emit(EventJobPanicked, map[string]any{
		"job_id": spec.JobID,
		"kind":   spec.Kind,
		"panic":  fmt.Sprint(cause),
	})
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
		m.runPatch(ctx, spec, progress)
	default:
		m.unsupportedJob(spec, progress)
	}
}

// runPatch fills in the dependencies the module can supply itself before
// handing over. PatchDeps publishes nothing when its Runner is nil, and a
// patch job that publishes nothing is a job the console shows as running
// forever; a half-wired build must still produce a terminal result.
func (m *Module) runPatch(ctx context.Context, spec scripts.JobSpec, progress scripts.ProgressFunc) {
	deps := m.Patch
	if deps.Runner == nil {
		deps.Runner = m.Scripts
	}
	if deps.Audit == nil {
		deps.Audit = m.Audit
	}
	if deps.Log == nil {
		deps.Log = m.Log
	}
	deps.Execute(ctx, spec, progress)
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
//
// The scheduler's context is deliberately not the job's parent. Shutdown is
// handled once, by the module, so a scheduled job is not cut off the moment
// the scheduler task notices the signal.
func (m *Module) RunScheduled(_ context.Context, jobID string, entry sched.Entry, fireAt time.Time) {
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
	// Scheduled runs take a worker slot and a registry entry like any other
	// job, so they are cancellable, counted against the concurrency cap, and
	// reported at shutdown. ctx is the scheduler's; the job's own context
	// comes from the registry.
	jobCtx, release, err := m.reserve(jobID, spec.Kind)
	if err != nil {
		m.Log.Warn("scheduled job not started", "job_id", jobID,
			"entry_id", entry.EntryID, "err", err)
		return
	}
	defer release()

	m.Log.Info("running scheduled job", "job_id", jobID, "entry_id", entry.EntryID,
		"kind", spec.Kind, "scheduled_for", fireAt.UTC())
	m.runJob(jobCtx, spec)
}
