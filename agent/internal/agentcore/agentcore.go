// Package agentcore supervises the periodic publishers. Each task runs in its
// own goroutine with panic recovery; a task that dies is restarted with
// exponential backoff, and everything stops cleanly on ctx cancel.
package agentcore

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/openrmm/agent/internal/heartbeat"
	"github.com/openrmm/agent/internal/inventory"
	"github.com/openrmm/agent/internal/telemetry"
)

const (
	initialBackoff = time.Second
	maxBackoff     = time.Minute
)

// Supervisor wires the publishers to a shared NATS connection.
type Supervisor struct {
	NC      *nats.Conn
	AgentID string
	Version string
	Log     *slog.Logger

	wg sync.WaitGroup
}

type task struct {
	name string
	run  func(context.Context, *slog.Logger) error
}

// Start launches heartbeat, telemetry, and inventory. It returns immediately;
// cancel ctx to stop, then Wait for the goroutines to drain.
func (s *Supervisor) Start(ctx context.Context) {
	tasks := []task{
		{"heartbeat", func(ctx context.Context, log *slog.Logger) error {
			return heartbeat.Run(ctx, s.NC, s.AgentID, s.Version, log)
		}},
		{"telemetry", func(ctx context.Context, log *slog.Logger) error {
			return telemetry.Run(ctx, s.NC, s.AgentID, log)
		}},
		{"inventory", func(ctx context.Context, log *slog.Logger) error {
			return inventory.Run(ctx, s.NC, s.AgentID, log)
		}},
	}
	for _, t := range tasks {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.supervise(ctx, t)
		}()
	}
}

// Wait blocks until every task goroutine has exited.
func (s *Supervisor) Wait() { s.wg.Wait() }

func (s *Supervisor) supervise(ctx context.Context, t task) {
	log := s.Log.With("task", t.name)
	backoff := initialBackoff
	for {
		start := time.Now()
		err := runRecovered(ctx, t, log)
		if ctx.Err() != nil {
			log.Info("task stopped")
			return
		}
		if time.Since(start) > maxBackoff {
			backoff = initialBackoff // it ran a while; treat the crash as fresh
		}
		log.Error("task died, restarting", "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runRecovered runs one task life, converting panics into errors so a bad
// collector can't take the whole agent down.
func runRecovered(ctx context.Context, t task, log *slog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return t.run(ctx, log)
}
