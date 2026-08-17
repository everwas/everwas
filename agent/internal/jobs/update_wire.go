package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/update"
)

// EventAgentUpdated is emitted for every update attempt, applied or refused.
// It belongs next to the other names in internal/audit; it lives here until
// that constant block is extended, same as the patch events.
const EventAgentUpdated = "agent.updated"

// updateJobTimeout bounds the whole download-verify-swap pipeline. An agent
// binary is tens of megabytes over a link that may be a branch office DSL
// line, so this is generous, but it is not unbounded: a stalled download
// must not hold a worker slot forever.
const updateJobTimeout = 30 * time.Minute

// Exit codes that tell the server WHY an update did not happen, so a ring
// rollout can distinguish "this host is already there" from "this artifact
// is bad" without parsing prose.
const (
	exitAlreadyCurrent  = 64 // nothing to do; not a failure
	exitVersionDenied   = 65 // this host rolled this version back already
	exitFinalizePending = 66 // a previous update is still finalizing
)

// UpdateDeps is everything the agent.update job needs, injected rather than
// reached for, so the handler is testable without touching the filesystem or
// restarting anything.
type UpdateDeps struct {
	StateDir string
	Version  string

	// Runner publishes job progress, output and results.
	Runner *scripts.Runner

	Audit *audit.Publisher
	Log   *slog.Logger

	// Apply defaults to update.Apply. Tests replace it.
	Apply func(context.Context, update.Request, update.Options) (*update.Result, error)

	// Restart asks the process to exit so the service manager starts the new
	// binary. Without it a "successful" update swaps the file on disk and
	// then goes on running the old code indefinitely, and the server is told
	// the host is updated when it is not. A nil Restart therefore REFUSES
	// the job rather than doing the swap and lying about the outcome.
	Restart func(reason string)

	// Handoff exits without requesting a restart, for the Windows finalizer
	// case. The finalizer's FIRST action is to wait for this process to exit
	// before it can swap a binary that is currently mapped, so not exiting
	// means it waits out its timeout and the update never lands.
	Handoff func(reason string)
}

// ready reports whether an update can be attempted at all. It is checked
// before the job is accepted so a refusal is an immediate reply rather than
// a job that starts and then fails.
func (d UpdateDeps) ready() error {
	if d.StateDir == "" {
		return errors.New("self-update is not wired: no state dir")
	}
	if d.Restart == nil {
		return errors.New("self-update is not wired: no restart hook")
	}
	return nil
}

func (d UpdateDeps) apply() func(context.Context, update.Request, update.Options) (*update.Result, error) {
	if d.Apply != nil {
		return d.Apply
	}
	return update.Apply
}

// runAgentUpdate is the execute() case for KindAgentUpdate. The update
// request rides in spec.Body as JSON, the same way patch ids do.
func (m *Module) runAgentUpdate(ctx context.Context, spec scripts.JobSpec, progress scripts.ProgressFunc) {
	deps := m.Update
	if deps.Runner == nil {
		deps.Runner = m.Scripts
	}
	if deps.Audit == nil {
		deps.Audit = m.Audit
	}
	if deps.Log == nil {
		deps.Log = m.Log
	}
	if deps.Version == "" {
		deps.Version = m.Version
	}
	deps.Execute(ctx, spec, progress)
}

// Execute runs one agent.update job end to end: progress, a human-readable
// line on the output stream, the terminal result and an audit event. Like
// the patch handler it never returns an error, because every failure mode
// is a Result the server can display.
//
// The restart is requested AFTER the result is published. A process that
// exits first leaves the server with a job that never reached a terminal
// state, which reads identically to an agent that died mid-update.
func (d UpdateDeps) Execute(ctx context.Context, spec scripts.JobSpec, progress scripts.ProgressFunc) scripts.Result {
	if progress == nil {
		progress = func(int, string, string) {}
	}
	started := time.Now()
	progress(0, scripts.PhaseStarted, spec.Kind)

	var req update.Request
	if err := json.Unmarshal([]byte(spec.Body), &req); err != nil {
		return d.finish(spec, progress, started, scripts.Result{
			Status: scripts.StatusFailed, ExitCode: -1,
		}, "agent update: bad request payload: "+err.Error(), req, nil)
	}

	if err := d.ready(); err != nil {
		return d.finish(spec, progress, started, scripts.Result{
			Status: scripts.StatusFailed, ExitCode: -1,
		}, "agent update: "+err.Error(), req, nil)
	}

	ctx, cancel := context.WithTimeout(ctx, updateJobTimeout)
	defer cancel()

	progress(10, scripts.PhaseRunning, "downloading "+req.Version)
	out, err := d.apply()(ctx, req, update.Options{
		StateDir:       d.StateDir,
		CurrentVersion: d.Version,
		Log:            d.Log,
	})

	res, summary := updateOutcome(d.Version, req, out, err)
	return d.finish(spec, progress, started, res, summary, req, out)
}

// finish publishes the summary, the terminal result and the audit event, and
// only then asks for the restart.
func (d UpdateDeps) finish(
	spec scripts.JobSpec,
	progress scripts.ProgressFunc,
	started time.Time,
	res scripts.Result,
	summary string,
	req update.Request,
	out *update.Result,
) scripts.Result {
	res.DurationMS = time.Since(started).Milliseconds()

	if d.Runner != nil {
		// One PublishStderr call per job: it emits both EOF markers, so a
		// second call would reopen a stream the server has already closed.
		d.Runner.PublishStderr(spec, "openrmm-agent: "+summary+"\n")
	}
	progress(100, scripts.PhaseFinished, res.Status)
	if d.Runner != nil {
		d.Runner.PublishResult(spec, res)
	}
	if d.Audit != nil {
		d.Audit.Emit(EventAgentUpdated, map[string]any{
			"job_id":       spec.JobID,
			"from_version": d.Version,
			"to_version":   req.Version,
			"status":       res.Status,
			"finalizing":   res.Finalizing,
			"requested_by": spec.RequestedBy,
			"summary":      summary,
		})
	}
	if d.Log != nil {
		d.Log.Info("agent update finished", "job_id", spec.JobID,
			"to_version", req.Version, "status", res.Status, "summary", summary)
	}

	// Exit after publishing the result, by whichever route fits.
	//
	// The finalizing case used to do nothing at all, on the reasoning that the
	// helper "restarts the process itself when it is done" and racing it would
	// kill the agent mid-swap. That has it backwards: SpawnFinalizer passes
	// this process's own pid and the helper's first action is to wait for it to
	// exit, precisely because the swap cannot touch a mapped binary. Nothing
	// asked this process to exit, so the helper waited out its two minutes,
	// wrote finalize_error, and the host stayed on the old version. The Windows
	// fallback could not succeed by construction.
	if out == nil || res.Status != scripts.StatusSucceeded {
		return res
	}
	switch {
	case res.Finalizing && d.Handoff != nil:
		d.Handoff("update finalizer is waiting to swap to " + req.Version)
	case !res.Finalizing && d.Restart != nil:
		d.Restart("updated to " + req.Version)
	}
	return res
}

// updateOutcome maps the pipeline's error onto the wire result. The three
// "did not update, and that is fine" cases are succeeded-with-a-code rather
// than failed: a ring rollout that treats "already running this version" as
// a failure stalls on the hosts that are already done.
func updateOutcome(current string, req update.Request, out *update.Result, err error) (scripts.Result, string) {
	switch {
	case errors.Is(err, update.ErrAlreadyCurrent):
		return scripts.Result{
				Status: scripts.StatusSucceeded, ExitCode: exitAlreadyCurrent, UpdatedTo: current,
			},
			fmt.Sprintf("agent update: already running %s, nothing to do", current)

	case errors.Is(err, update.ErrVersionDenied):
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: exitVersionDenied},
			fmt.Sprintf("agent update refused: this host rolled %s back already; "+
				"send force to override", req.Version)

	case errors.Is(err, update.ErrFinalizePending):
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: exitFinalizePending},
			"agent update refused: a previous update is still finalizing"

	case errors.Is(err, context.DeadlineExceeded):
		return scripts.Result{Status: scripts.StatusTimeout, ExitCode: -1},
			"agent update timed out: " + errText(err)

	case errors.Is(err, context.Canceled):
		return scripts.Result{Status: scripts.StatusCancelled, ExitCode: -1},
			"agent update cancelled"

	case err != nil:
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: -1},
			"agent update failed: " + errText(err)

	case out == nil:
		// Defensive: a nil result with a nil error would otherwise report
		// success for an update that demonstrably did not happen.
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: -1},
			"agent update failed: the update pipeline reported neither a result nor an error"

	case out.Finalizing:
		return scripts.Result{
				Status:     scripts.StatusSucceeded,
				UpdatedTo:  out.Version,
				Finalizing: true,
			},
			fmt.Sprintf("agent update staged %s; a helper process (pid %d) completes the swap "+
				"after this one exits, so the host is still on %s until it reports back",
				out.Version, out.FinalizerPID, current)

	default:
		return scripts.Result{Status: scripts.StatusSucceeded, UpdatedTo: out.Version},
			fmt.Sprintf("agent update applied %s -> %s; restarting", current, out.Version)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
