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

	"github.com/openrmm/agent/internal/audit"
)

const (
	// DefaultTimeout applies when the job spec does not set timeout_s.
	DefaultTimeout = 300 * time.Second
	// MaxTimeout bounds a server-supplied timeout; anything longer is a bug
	// or an attack on the agent's job slot.
	MaxTimeout = 24 * time.Hour

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
}

// Result is the terminal message published on …jobs.{job_id}.result.
type Result struct {
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
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

	// emit publishes one output chunk. Tests replace it to observe framing
	// without standing up a NATS server.
	emit func(Chunk) error

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
	sink := newChunkSink(job.JobID, r.chunkOut)

	res := r.execute(ctx, job, publish, sink)
	res.DurationMS = time.Since(started).Milliseconds()
	res.Truncated = sink.isTruncated()

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

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return r.abort(sink, job, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return r.abort(sink, job, err)
	}

	guard := newProcGuard()
	guard.beforeStart(cmd)
	if err := cmd.Start(); err != nil {
		return r.abort(sink, job, fmt.Errorf("start %s: %w", interp.Path, err))
	}
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

	// Drain both pipes fully before Wait: Wait closes them, and a reader
	// still running when that happens loses the tail of the output.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.pump(sink, StreamStdout, stdout, job.JobID) }()
	go func() { defer wg.Done(); r.pump(sink, StreamStderr, stderr, job.JobID) }()
	wg.Wait()

	waitErr := cmd.Wait()
	close(done)

	return result(waitErr, reason.get())
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
	if werr := sink.write(StreamStderr, []byte("openrmm-agent: "+err.Error()+"\n")); werr != nil {
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
	return r.publishChunk(c)
}

func (r *Runner) warn(msg string, args ...any) {
	if r.Log != nil {
		r.Log.Warn(msg, args...)
	}
}
