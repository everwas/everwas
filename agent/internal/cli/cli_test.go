package cli

import (
	"context"
	"testing"
	"time"
)

// never is a restart channel nothing ever sends on.
func never() chan string { return make(chan string) }

// TestWaitForShutdownTellsCrashFromShutdown pins the difference cmdRun's exit
// code depends on. A closed NATS connection is not recoverable, so the agent
// has to exit non-zero and let the service manager restart it; a SIGTERM is
// an ordinary stop and must exit zero, or every clean restart looks like a
// crash to the supervisor.
func TestWaitForShutdownTellsCrashFromShutdown(t *testing.T) {
	t.Run("a closed connection is fatal", func(t *testing.T) {
		deaf := make(chan struct{})
		close(deaf)
		if !waitForShutdown(context.Background(), deaf, never()).deaf {
			t.Error("a deaf agent was treated as a clean shutdown, so nothing would restart it")
		}
	})

	t.Run("a signal is not", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if waitForShutdown(ctx, make(chan struct{}), never()).deaf {
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
			if waitForShutdown(ctx, deaf, never()).deaf {
				t.Fatal("a clean shutdown was reported as a crash")
			}
		}
	})

	t.Run("it blocks while the agent is healthy", func(t *testing.T) {
		done := make(chan stopReason, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { done <- waitForShutdown(ctx, make(chan struct{}), never()) }()
		select {
		case <-done:
			t.Error("waitForShutdown returned while the agent was fine")
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestRestartIsNotACrash covers the self-update path. A swapped binary only
// takes effect when the process exits, but exiting non-zero would make the
// rollback tracker count a crash against a version that is working fine, and
// enough of those roll it back.
func TestRestartIsNotACrash(t *testing.T) {
	restart := make(chan string, 1)
	restart <- "updated to 2026.8.16"

	why := waitForShutdown(context.Background(), make(chan struct{}), restart)
	if why.deaf {
		t.Fatal("a self-update restart was reported as a deaf connection, which exits non-zero " +
			"and counts as a crash against the new version")
	}
	if why.restart != "updated to 2026.8.16" {
		t.Fatalf("restart reason lost: %q", why.restart)
	}
}
