package shell

import "sync"

// RingBytes is how much PTY output we hold while NATS is disconnected.
const RingBytes = 256 * 1024

// ring is a drop-oldest byte buffer. Terminal output is only useful in its
// most recent form, so when the buffer is full the front is discarded and
// the session reports a gap rather than replaying stale scrollback.
type ring struct {
	max int

	mu      sync.Mutex
	buf     []byte
	dropped bool
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) write(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.max {
		r.buf = append(r.buf[:0], p[len(p)-r.max:]...)
		r.dropped = true
		return
	}
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
		r.dropped = true
	}
}

// drain returns the buffered bytes and whether anything was dropped, and
// resets both.
func (r *ring) drain() ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out, dropped := r.buf, r.dropped
	r.buf, r.dropped = nil, false
	return out, dropped
}

func (r *ring) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}
