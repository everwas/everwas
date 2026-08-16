package cli

import (
	"context"
	"testing"
	"time"
)

// TestWaitForShutdownTellsCrashFromShutdown pins the difference cmdRun's exit
// code depends on. A closed NATS connection is not recoverable, so the agent
// has to exit non-zero and let the service manager restart it; a SIGTERM is
// an ordinary stop and must exit zero, or every clean restart looks like a
// crash to the supervisor.
func TestWaitForShutdownTellsCrashFromShutdown(t *testing.T) {
	t.Run("a closed connection is fatal", func(t *testing.T) {
		deaf := make(chan struct{})
		close(deaf)
		if !waitForShutdown(context.Background(), deaf) {
			t.Error("a deaf agent was treated as a clean shutdown, so nothing would restart it")
		}
	})

	t.Run("a signal is not", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if waitForShutdown(ctx, make(chan struct{})) {
			t.Error("an ordinary shutdown was reported as a crash")
		}
	})

	t.Run("a signal wins a tie", func(t *testing.T) {
		// Both ready at once: draining on shutdown closes the connection,
		// which fires the same callback. Reported as a crash it would exit 1
		// on every clean stop.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		deaf := make(chan struct{})
		close(deaf)
		for i := 0; i < 100; i++ {
			if waitForShutdown(ctx, deaf) {
				t.Fatal("a clean shutdown was reported as a crash")
			}
		}
	})

	t.Run("it blocks while the agent is healthy", func(t *testing.T) {
		done := make(chan bool, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { done <- waitForShutdown(ctx, make(chan struct{})) }()
		select {
		case <-done:
			t.Error("waitForShutdown returned while the agent was fine")
		case <-time.After(100 * time.Millisecond):
		}
	})
}
