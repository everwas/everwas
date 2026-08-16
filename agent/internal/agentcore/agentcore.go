// Package agentcore supervises the agent's modules. Each task runs in its
// own goroutine with panic recovery; a task that dies is restarted with
// exponential backoff, and everything stops cleanly on ctx cancel.
package agentcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/heartbeat"
	"github.com/rsp2k/openrmm/agent/internal/inventory"
	"github.com/rsp2k/openrmm/agent/internal/jobs"
	"github.com/rsp2k/openrmm/agent/internal/sched"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/shell"
	"github.com/rsp2k/openrmm/agent/internal/telemetry"
	"github.com/rsp2k/openrmm/agent/internal/update"
)

const (
	initialBackoff = time.Second
	maxBackoff     = time.Minute
)

// Supervisor wires the modules to a shared NATS connection.
type Supervisor struct {
	NC       *nats.Conn
	AgentID  string
	Version  string
	StateDir string
	Log      *slog.Logger

	wg sync.WaitGroup
}

type task struct {
	name string
	run  func(context.Context, *slog.Logger) error
}

// Start builds the modules and launches every task. It returns immediately;
// cancel ctx to stop, then Wait for the goroutines to drain.
func (s *Supervisor) Start(ctx context.Context) {
	aud := audit.New(s.NC, s.AgentID, s.Log)
	shells := shell.New(s.NC, s.AgentID, aud, s.Log)
	runner := scripts.NewRunner(s.NC, s.AgentID, s.StateDir, aud, s.Log)
	patches := inventory.NewPatchCollector(s.NC, s.AgentID, s.Log)

	jobsMod := &jobs.Module{
		NC:      s.NC,
		AgentID: s.AgentID,
		Version: s.Version,
		Log:     s.Log,
		Shell:   shells,
		Scripts: runner,
		Audit:   aud,
		RefreshInventory: func(ctx context.Context) error {
			return inventory.RefreshNow(ctx, s.NC, s.AgentID, s.Log)
		},
		Patch: jobs.PatchDeps{
			Manager:           patches.Manager,
			RefreshPatchState: patches.RefreshNow,
			Runner:            runner,
			Audit:             aud,
			Log:               s.Log,
		},
	}
	scheduler := sched.New(s.AgentID, s.StateDir, jobsMod.RunScheduled, aud, s.Log)
	jobsMod.Sched = scheduler

	tasks := []task{
		{"heartbeat", func(ctx context.Context, log *slog.Logger) error {
			return heartbeat.Run(ctx, s.NC, s.AgentID, s.Version, scheduler.Version, log)
		}},
		{"telemetry", func(ctx context.Context, log *slog.Logger) error {
			return telemetry.Run(ctx, s.NC, s.AgentID, log)
		}},
		{"inventory", func(ctx context.Context, log *slog.Logger) error {
			return inventory.Run(ctx, s.NC, s.AgentID, log)
		}},
		{"shell", func(ctx context.Context, _ *slog.Logger) error {
			return shells.Run(ctx)
		}},
		{"jobs", func(ctx context.Context, _ *slog.Logger) error {
			return jobsMod.Run(ctx)
		}},
		{"sched", func(ctx context.Context, _ *slog.Logger) error {
			return scheduler.Run(ctx)
		}},
		{"patchstate", func(ctx context.Context, _ *slog.Logger) error {
			return patches.Run(ctx)
		}},
		// Confirms a freshly updated build actually WORKS before the previous
		// one stops being the fallback. "Stayed alive 60 seconds" was not
		// evidence: the supervisor restarts crashed tasks forever, so a build
		// whose jobs module panicked on every start still cleared that bar.
		// The flush proves the connection answers in both directions; the
		// patchstate publish proves a real unit of work completes end to end.
		{"update-health", func(ctx context.Context, log *slog.Logger) error {
			return update.Watch(ctx, update.WatchConfig{
				StateDir: s.StateDir,
				Log:      log,
				Probe: func(ctx context.Context) error {
					ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					if !s.NC.IsConnected() {
						return errors.New("nats connection is not established")
					}
					if err := s.NC.FlushWithContext(ctx); err != nil {
						return fmt.Errorf("nats round trip: %w", err)
					}
					if _, err := patches.RefreshNow(ctx); err != nil {
						return fmt.Errorf("patchstate publish: %w", err)
					}
					return nil
				},
			})
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
