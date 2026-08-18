// Package scripts executes server-dispatched scripts: it stages the body in
// a private 0700 directory, runs it under the right interpreter with a
// scrubbed environment, streams stdout/stderr as chunks, and reports a
// terminal result. Timeouts kill the whole process group.
package scripts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/everwas/everwas/agent/internal/audit"
)

const (
	// DefaultTimeout applies when the job spec does not set timeout_s.
	DefaultTimeout = 300 * time.Second
	// MaxTimeout bounds a server-supplied timeout; anything longer is a bug
	// or an attack on the agent's job slot.
	MaxTimeout = 24 * time.Hour

	// DefaultDrainGrace is how long the output pipes get to deliver what is
	// still buffered after the child exits, before we stop reading them.
	DefaultDrainGrace = 3 * time.Second

	readBufSize = 32 * 1024
)

// Terminal job statuses.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// Job kinds carried in a job envelope.
const (
	KindScriptRun        = "script.run"
	KindInventoryRefresh = "inventory.refresh"
	KindPatchScan        = "patch.scan"
	KindPatchInstall     = "patch.install"
	KindAgentUpdate      = "agent.update"
)

// Progress phases.
const (
	PhaseStarted  = "started"
	PhaseRunning  = "running"
	PhaseFinished = "finished"
)

// JobSpec is the data payload of a job envelope.
type JobSpec struct {
	JobID       string            `json:"job_id"`
	Kind        string            `json:"kind"`
	Shell       string            `json:"shell"`
	Body        string            `json:"body"`
	TimeoutS    int               `json:"timeout_s"`
	Env         map[string]string `json:"env"`
	RequestedBy string            `json:"requested_by"`

	// EntryID is set only for scheduled runs. The server never assigned this
	// job an id, because the fire came out of the agent's own cache while it
	// may well have been offline, so the entry is the only thing that says
	// WHICH schedule this result belongs to.
	EntryID string `json:"entry_id,omitempty"`
}

// Result is the terminal message published on …jobs.{job_id}.result.
type Result struct {
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`

	// Patch jobs only; omitted entirely by script jobs.
	//
	// Without these the server records a patch job as "succeeded" with an
	// empty installed list, so an operator cannot tell WHICH updates landed
	// from the authoritative job record. The audit event carries the same
	// facts, but job state must not depend on a separate best-effort stream.
	Installed      []string          `json:"installed,omitempty"`
	Failed         map[string]string `json:"failed,omitempty"`
	RebootRequired bool              `json:"reboot_required,omitempty"`

	// Scheduled runs only. Echoed straight back from the job spec: the
	// server has no row for this run yet and cannot look one up by job id, so
	// without this the result arrives for an id nothing knows about and is
	// dropped as an unknown run.
	EntryID string `json:"entry_id,omitempty"`

	// agent.update jobs only.
	//
	// Finalizing is NOT success. On Windows the swap is handed to a helper
	// process that finishes after this one exits, so the host is still on the
	// old binary when this result is published. A server that reads a
	// finalizing job as "updated" moves the ring forward over a fleet that
	// has not actually changed version.
	UpdatedTo  string `json:"updated_to,omitempty"`
	Finalizing bool   `json:"finalizing,omitempty"`
}

// ProgressFunc reports a phase transition for a job.
type ProgressFunc func(pct int, phase, note string)

// Runner executes jobs and tracks the running ones so they can be cancelled.
type Runner struct {
	NC       *nats.Conn
	AgentID  string
	StateDir string
	Audit    *audit.Publisher
	Log      *slog.Logger

	// OnResult and OnChunk intercept publishing. Set by tests in other
	// packages, which cannot reach the unexported emit hook below.
	OnResult func(jobID string, res Result)
	OnChunk  func(Chunk) error

	// emit publishes one output chunk. Tests replace it to observe framing
	// without standing up a NATS server.
	emit func(Chunk) error

	// drainGrace overrides DefaultDrainGrace; tests shorten it.
	drainGrace time.Duration

	// results publishes terminal results with an ack. Built from NC on
	// first use; tests inject a fake.
	resultsOnce sync.Once
	results     resultPublisher

	mu      sync.Mutex
	running map[string]*handle
}

type handle struct {
	stop func(reason string)
}

// NewRunner builds a Runner. StateDir is where script bodies are staged.
func NewRunner(nc *nats.Conn, agentID, stateDir string, aud *audit.Publisher, log *slog.Logger) *Runner {
	return &Runner{
		NC:       nc,
		AgentID:  agentID,
		StateDir: stateDir,
		Audit:    aud,
		Log:      log,
		running:  map[string]*handle{},
	}
}

// Run executes one job to completion, streaming output and publishing the
// terminal result. It never returns an error: every failure mode is a Result
// the server can display.
func (r *Runner) Run(ctx context.Context, job JobSpec, publish ProgressFunc) Result {
	if publish == nil {
		publish = func(int, string, string) {}
	}
	started := time.Now()
	sink := newChunkSink(job.JobID, job.EntryID, r.chunkOut)

	res := r.execute(ctx, job, publish, sink)
	res.DurationMS = time.Since(started).Milliseconds()
	res.Truncated = sink.isTruncated()
	res.EntryID = job.EntryID

	if err := sink.eof(StreamStdout); err != nil {
		r.warn("job output eof", "job_id", job.JobID, "err", err)
	}
	if err := sink.eof(StreamStderr); err != nil {
		r.warn("job output eof", "job_id", job.JobID, "err", err)
	}
	publish(100, PhaseFinished, res.Status)
	r.publishResult(job.JobID, res)

	sum := sha256.Sum256([]byte(job.Body))
	r.Audit.Emit(audit.ScriptExecuted, map[string]any{
		"job_id":       job.JobID,
		"shell":        job.Shell,
		"sha256":       hex.EncodeToString(sum[:]),
		"status":       res.Status,
		"exit_code":    res.ExitCode,
		"duration_ms":  res.DurationMS,
		"truncated":    res.Truncated,
		"requested_by": job.RequestedBy,
	})
	return res
}

// Cancel kills a running job. It reports whether the job was found.
func (r *Runner) Cancel(jobID string) bool {
	r.mu.Lock()
	h, ok := r.running[jobID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	h.stop(StatusCancelled)
	return true
}

// Running lists the job ids currently executing.
func (r *Runner) Running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.running))
	for id := range r.running {
		ids = append(ids, id)
	}
	return ids
}

func (r *Runner) register(jobID string, stop func(string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running[jobID] = &handle{stop: stop}
}

func (r *Runner) unregister(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, jobID)
}

func (r *Runner) execute(ctx context.Context, job JobSpec, publish ProgressFunc, sink *chunkSink) Result {
	publish(0, PhaseStarted, job.Shell)

	if job.Body == "" {
		return r.abort(sink, job, errors.New("empty script body"))
	}
	interp, err := Resolve(job.Shell)
	if err != nil {
		return r.abort(sink, job, err)
	}
	dir, script, err := stageScript(r.StateDir, job.JobID, job.Body, interp.Ext)
	if err != nil {
		return r.abort(sink, job, fmt.Errorf("stage script: %w", err))
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			r.warn("remove job workdir", "job_id", job.JobID, "err", err)
		}
	}()

	argv := interp.Argv(script)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = scrubEnv(os.Environ(), job.Env)

	// Our own pipes rather than cmd.StdoutPipe: Wait closes the pipes it
	// owns the instant the child exits, which races the readers and eats the
	// tail of the output. Owning them means we decide when reading stops.
	pipes, err := newOutPipes()
	if err != nil {
		return r.abort(sink, job, err)
	}
	defer pipes.closeAll()
	cmd.Stdout, cmd.Stderr = pipes.outW, pipes.errW

	guard := newProcGuard()
	guard.beforeStart(cmd)
	if err := cmd.Start(); err != nil {
		return r.abort(sink, job, fmt.Errorf("start %s: %w", interp.Path, err))
	}
	// This process holds a copy of both write ends; without dropping them the
	// reads below could never see EOF even for a well-behaved script.
	pipes.closeWrite()
	guard.afterStart(cmd)
	defer guard.release()
	publish(10, PhaseRunning, interp.Path)

	var reason reasonBox
	kill := func(why string) {
		if reason.set(why) {
			if err := guard.kill(cmd); err != nil {
				r.warn("kill job", "job_id", job.JobID, "err", err)
			}
		}
	}
	r.register(job.JobID, kill)
	defer r.unregister(job.JobID)

	done := make(chan struct{})
	timer := time.NewTimer(jobTimeout(job.TimeoutS))
	defer timer.Stop()
	go func() {
		select {
		case <-done:
		case <-timer.C:
			kill(StatusTimeout)
		case <-ctx.Done():
			kill(StatusCancelled)
		}
	}()

	// The result comes from Wait, never from the pipes reaching EOF. Any
	// descendant that called setsid — a daemon the script installed and
	// started, gpg's dirmngr, ssh-agent — inherits the write end and is not
	// in the process group the timeout kills, so EOF may never arrive.
	// Waiting for it means no result, no timeout, no cleanup: a job the
	// console shows as running forever and a staged script body, often
	// carrying credentials, left on disk.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.pump(sink, StreamStdout, pipes.outR, job.JobID) }()
	go func() { defer wg.Done(); r.pump(sink, StreamStderr, pipes.errR, job.JobID) }()
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()

	waitErr := cmd.Wait()
	close(done)

	// The child is gone. Give the pipes a bounded moment to deliver what is
	// still buffered, then stop reading them and report. Losing the tail of
	// a job's output is strictly better than never reporting the job.
	select {
	case <-drained:
	case <-time.After(r.drainWindow()):
		r.warn("job output pipe held open past exit, abandoning the tail",
			"job_id", job.JobID, "grace", r.drainWindow().String())
		sink.stop()
		pipes.closeRead()
	}
	return result(waitErr, reason.get())
}

// drainWindow is how long the pumps get after the child exits.
func (r *Runner) drainWindow() time.Duration {
	if r.drainGrace > 0 {
		return r.drainGrace
	}
	return DefaultDrainGrace
}

// result maps a wait error plus any kill reason onto the wire status.
func result(waitErr error, reason string) Result {
	exit := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	switch {
	case reason != "":
		if exit == 0 {
			exit = -1 // killed processes sometimes report 0 on some platforms
		}
		return Result{Status: reason, ExitCode: exit}
	case exit != 0:
		return Result{Status: StatusFailed, ExitCode: exit}
	default:
		return Result{Status: StatusSucceeded, ExitCode: 0}
	}
}

// abort reports a failure that happened before (or instead of) execution.
// The reason goes out on stderr so an operator sees it in the console.
func (r *Runner) abort(sink *chunkSink, job JobSpec, err error) Result {
	if werr := sink.write(StreamStderr, []byte("everwas-agent: "+err.Error()+"\n")); werr != nil {
		r.warn("job stderr write", "job_id", job.JobID, "err", werr)
	}
	r.warn("job aborted", "job_id", job.JobID, "err", err)
	return Result{Status: StatusFailed, ExitCode: -1}
}

func (r *Runner) pump(sink *chunkSink, stream string, rc io.Reader, jobID string) {
	buf := make([]byte, readBufSize)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if werr := sink.write(stream, buf[:n]); werr != nil {
				r.warn("job output write", "job_id", jobID, "stream", stream, "err", werr)
			}
		}
		if err != nil {
			return // io.EOF or the pipe closing under a kill
		}
	}
}

// jobTimeout clamps the server-provided timeout into a sane range.
func jobTimeout(sec int) time.Duration {
	if sec <= 0 {
		return DefaultTimeout
	}
	if d := time.Duration(sec) * time.Second; d < MaxTimeout {
		return d
	}
	return MaxTimeout
}

// reasonBox records the first reason a job was killed; later reasons lose.
type reasonBox struct {
	mu     sync.Mutex
	reason string
}

func (b *reasonBox) set(reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reason != "" {
		return false
	}
	b.reason = reason
	return true
}

func (b *reasonBox) get() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason
}

// chunkOut is the sink's emit hook, indirected so tests can capture output.
func (r *Runner) chunkOut(c Chunk) error {
	if r.emit != nil {
		return r.emit(c)
	}
	if r.OnChunk != nil {
		return r.OnChunk(c)
	}
	return r.publishChunk(c)
}

func (r *Runner) warn(msg string, args ...any) {
	if r.Log != nil {
		r.Log.Warn(msg, args...)
	}
}
