package update

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrStillRunning means the old process outlived the wait.
var ErrStillRunning = errors.New("update: process is still running")

const procPollInterval = 250 * time.Millisecond

// WaitForExit polls until the process with the given pid is gone, ctx is
// cancelled, or timeout elapses. Polling beats a handle wait here because the
// finalizer may be watching a process it did not spawn and cannot reap.
func WaitForExit(ctx context.Context, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ProcessExited(pid) {
			// Give the OS a moment to release the executable image. Windows
			// unmaps it slightly after the process object reports exited.
			select {
			case <-ctx.Done():
			case <-time.After(procPollInterval):
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: pid %d after %s", ErrStillRunning, pid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(procPollInterval):
		}
	}
}
