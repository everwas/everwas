package shell

import (
	"context"
	"sync"
)

const (
	// MaxFrameBytes is the largest PTY output frame we publish.
	MaxFrameBytes = 32 * 1024

	// PauseAbove is the un-acked byte count that stops PTY reads. Core NATS
	// drops messages for slow consumers, so a `yes` flood has to be stopped
	// at the source, not absorbed downstream.
	PauseAbove = 512 * 1024

	// ResumeBelow is the un-acked count that restarts reads. Resuming at
	// half the pause threshold keeps a busy session from flapping between
	// paused and running on every ack.
	ResumeBelow = 256 * 1024
)

// flowController tracks bytes published but not yet acknowledged by the
// server bridge. It has no knowledge of PTYs, which is what makes the
// accounting testable on its own.
type flowController struct {
	pauseAbove  int64
	resumeBelow int64

	mu      sync.Mutex
	unacked int64
	paused  bool
	gate    chan struct{} // non-nil while paused; closed on resume
}

func newFlowController() *flowController {
	return newFlowControllerWith(PauseAbove, ResumeBelow)
}

func newFlowControllerWith(pauseAbove, resumeBelow int64) *flowController {
	return &flowController{pauseAbove: pauseAbove, resumeBelow: resumeBelow}
}

// sent records n published bytes and reports whether reads are now paused.
func (f *flowController) sent(n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unacked += int64(n)
	if !f.paused && f.unacked > f.pauseAbove {
		f.paused = true
		f.gate = make(chan struct{})
	}
	return f.paused
}

// ack records n acknowledged bytes and reports whether reads just resumed.
func (f *flowController) ack(n int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= 0 {
		return false
	}
	f.unacked -= n
	if f.unacked < 0 {
		f.unacked = 0 // a duplicated or over-counted ack must not go negative
	}
	if f.paused && f.unacked <= f.resumeBelow {
		f.paused = false
		close(f.gate)
		f.gate = nil
		return true
	}
	return false
}

// wait blocks while the session is paused.
func (f *flowController) wait(ctx context.Context) error {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *flowController) isPaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *flowController) unackedBytes() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unacked
}

// release unblocks any waiter, for teardown.
func (f *flowController) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paused {
		f.paused = false
		close(f.gate)
		f.gate = nil
	}
}
