package jobs

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/scripts"
)

const (
	// maxConcurrentJobs bounds how many jobs execute at once. JOBS retains a
	// week and the consumer reads from the start of the stream, so an agent
	// that was off for three days reconnects to every job queued in that
	// window at once. Unbounded dispatch turns that into two hundred shells
	// or package manager runs on one endpoint.
	maxConcurrentJobs = 3

	// defaultShutdownGrace is how long running jobs get to finish on their
	// own when the agent is asked to stop.
	defaultShutdownGrace = 30 * time.Second

	// defaultCancelGrace is how long we then wait after cancelling their
	// contexts before reporting whatever is left ourselves.
	defaultCancelGrace = 5 * time.Second
)

var (
	// errJobRunning marks a redelivery of a job that is already executing.
	errJobRunning = errors.New("job is already running")

	// errStopping means the agent is shutting down and must not start work.
	errStopping = errors.New("agent is shutting down")
)

// jobHandle is one running job's cancellation hook. The registry covers
// every kind of job, not just script runs: a patch install that registered
// nowhere could not be cancelled at all, and the API said so incorrectly.
type jobHandle struct {
	kind   string
	cancel context.CancelFunc
}

// ensure builds the registry lazily. The Module is a struct literal in the
// supervisor and in tests, so there is no constructor to do it in. Callers
// hold m.mu.
func (m *Module) ensure() {
	if m.inflight == nil {
		m.inflight = map[string]*jobHandle{}
	}
	if m.slots == nil {
		m.slots = make(chan struct{}, maxConcurrentJobs)
	}
	if m.base == nil {
		m.base, m.stopJobs = context.WithCancel(context.Background())
	}
}

// startJobs arms the registry for a consumer lifetime, replacing a job
// context left cancelled by a previous shutdown or crash.
func (m *Module) startJobs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensure()
	if m.stopping || m.base.Err() != nil {
		m.base, m.stopJobs = context.WithCancel(context.Background())
		m.stopping = false
	}
}

// reserve takes a worker slot and registers the job as in flight. It blocks
// while every slot is busy, which is the point: the caller has not acked
// yet, so MaxAckPending stops the server pushing more work at us.
//
// The returned release must be called exactly once when the job finishes.
func (m *Module) reserve(jobID, kind string) (context.Context, func(), error) {
	m.mu.Lock()
	m.ensure()
	switch {
	case m.stopping:
		m.mu.Unlock()
		return nil, nil, errStopping
	case m.inflight[jobID] != nil:
		m.mu.Unlock()
		return nil, nil, errJobRunning
	}
	base, slots := m.base, m.slots
	m.mu.Unlock()

	select {
	case slots <- struct{}{}:
	case <-base.Done():
		return nil, nil, errStopping
	}

	m.mu.Lock()
	// Re-check under the lock we just gave up while waiting for a slot.
	var err error
	switch {
	case m.stopping:
		err = errStopping
	case m.inflight[jobID] != nil:
		err = errJobRunning
	}
	if err != nil {
		m.mu.Unlock()
		<-slots
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(base)
	m.inflight[jobID] = &jobHandle{kind: kind, cancel: cancel}
	m.wg.Add(1)
	m.mu.Unlock()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			m.mu.Lock()
			delete(m.inflight, jobID)
			m.mu.Unlock()
			<-slots
			m.wg.Done()
		})
	}, nil
}

// CancelJob stops a running job of any kind and reports whether it found
// one. Every dispatched job is in the registry, so cancelling a four hour
// patch install works; it used to be refused with "job not running" because
// the only registry belonged to the script runner.
func (m *Module) CancelJob(jobID string) bool {
	m.mu.Lock()
	h := m.inflight[jobID]
	m.mu.Unlock()
	if h != nil {
		h.cancel()
	}
	// The script runner kills the process group immediately rather than
	// waiting for the job to notice its context.
	if m.Scripts != nil && m.Scripts.Cancel(jobID) {
		return true
	}
	return h != nil
}

// RunningJobs lists the job ids currently executing.
func (m *Module) RunningJobs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.inflight))
	for id := range m.inflight {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// drainJobs stops accepting work and gets everything still running to a
// terminal state before the process exits.
//
// The old shutdown abandoned in-flight jobs: NATS was drained and the
// process left while a script or a patch install was still going, so no
// result was ever published and the server showed the job running forever.
// Jobs get a grace period to finish, then their contexts are cancelled, and
// anything still alive after that is reported by us. A wrong-but-terminal
// result beats a job that never ends.
func (m *Module) drainJobs() {
	m.mu.Lock()
	m.stopping = true
	stop := m.stopJobs
	m.mu.Unlock()

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()

	grace, cancelIn := m.graces()
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	m.Log.Warn("jobs still running at shutdown, cancelling",
		"jobs", m.RunningJobs(), "grace", grace.String())
	if stop != nil {
		stop()
	}
	select {
	case <-done:
		return
	case <-time.After(cancelIn):
	}
	for _, id := range m.RunningJobs() {
		m.reportInterrupted(id)
	}
}

// graces returns the shutdown timings; tests shorten them.
func (m *Module) graces() (time.Duration, time.Duration) {
	grace, cancelIn := m.shutdownGrace, m.cancelGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	if cancelIn <= 0 {
		cancelIn = defaultCancelGrace
	}
	return grace, cancelIn
}

// reportInterrupted publishes a terminal result for a job the agent could
// not finish, so the server is never left guessing.
func (m *Module) reportInterrupted(jobID string) {
	m.Log.Warn("reporting a job the agent could not finish", "job_id", jobID)
	if m.Scripts == nil {
		return
	}
	m.Scripts.PublishStderr(jobID,
		"openrmm-agent: the agent stopped before this job finished\n")
	m.Scripts.PublishResult(jobID, scripts.Result{
		Status:   scripts.StatusCancelled,
		ExitCode: -1,
	})
}
