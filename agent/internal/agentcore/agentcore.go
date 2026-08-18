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

	"github.com/everwas/everwas/agent/internal/audit"
	"github.com/everwas/everwas/agent/internal/enroll"
	"github.com/everwas/everwas/agent/internal/heartbeat"
	"github.com/everwas/everwas/agent/internal/inventory"
	"github.com/everwas/everwas/agent/internal/jobs"
	"github.com/everwas/everwas/agent/internal/netcert"
	"github.com/everwas/everwas/agent/internal/sched"
	"github.com/everwas/everwas/agent/internal/scripts"
	"github.com/everwas/everwas/agent/internal/shell"
	"github.com/everwas/everwas/agent/internal/telemetry"
	"github.com/everwas/everwas/agent/internal/update"
)

const (
	initialBackoff = time.Second
	maxBackoff     = time.Minute
)

// renewLoop renews at startup and then on a fixed interval.
//
// A refusal is terminal and stops the loop: the credential we hold is not
// coming back (retired device, or a revocation window that closed while we
// were away), and retrying every twelve hours forever would bury the one log
// line that explains why this agent is about to go silent.
func renewLoop(ctx context.Context, renew func(context.Context) error, log *slog.Logger) error {
	attempt := func() bool {
		err := renew(ctx)
		switch {
		case err == nil:
			log.Info("agent credential renewed")
		case errors.Is(err, enroll.ErrRenewRefused):
			log.Error("credential renewal refused; this agent needs re-enrolling", "err", err)
			return false
		default:
			// Transient: the server is down, DNS is broken, we are offline.
			// The old credential still works, so there is time.
			log.Warn("credential renewal failed, will retry", "err", err)
		}
		return true
	}

	// No immediate attempt: runAgent renews before it connects, so that the
	// connection proves receipt. Starting here as well would renew twice on
	// every start and leave the second one unconfirmed.
	ticker := time.NewTicker(enroll.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !attempt() {
				return nil
			}
		}
	}
}

// netcertCheckEvery is how often the device re-examines its network
// certificate. Renewal starts at half of a ninety-day life, so twice a day is
// far more often than strictly needed; it is cheap because a certificate that
// is not yet due costs one file read and no network at all. The frequency buys
// two things: a server that has the CA enabled after the fleet was already
// deployed is picked up the same day without touching a single agent, and a
// machine that was switched off through its entire renewal window starts
// catching up within hours of coming back rather than at its next restart.
const netcertCheckEvery = 12 * time.Hour

// netcertUrgentEvery is the cadence once the certificate is past its urgent
// point. Borrowed from DHCP, where passing the rebinding timer changes the
// client's strategy rather than merely repeating it: at this point renewal has
// been failing for most of the margin and the deadline is close enough that
// twelve hours between attempts throws away most of the remaining chances.
const netcertUrgentEvery = time.Hour

// netcertLoop keeps the device's 802.1X certificate current.
//
// Unlike renewLoop this DOES attempt immediately, because there is no earlier
// step that already did it. The management credential is renewed before the
// NATS connection is made, so the connection itself proves receipt; a network
// certificate has no such proof available here and the machine may be holding
// nothing at all.
//
// The intervals are parameters rather than constants read from inside so a
// test can drive many cycles: with them hard-coded, any assertion about
// repeated behaviour observes exactly one startup attempt and passes whether
// or not the behaviour it claims to check exists.

// netcertConfig is a struct rather than six positional parameters because
// three of them are now durations and two are callbacks, and a call site with
// two adjacent time.Durations is one transposition away from a fleet that
// retries urgently forever and routinely never.
type netcertConfig struct {
	// ensure obtains or renews, and reports what the device holds afterwards.
	// On failure it still returns whatever is on disk, because the urgency of
	// the failure depends entirely on that.
	ensure func(context.Context) (*netcert.Material, error)
	// agentID picks this device's renewal offset within the fleet's spread.
	agentID string
	// every is the routine cadence; urgent applies past netcert.UrgentAt.
	every, urgent time.Duration
	// warnUser interrupts whoever is at the machine. Optional.
	warnUser func(context.Context, netcert.Phase, time.Time)
	// observe publishes what the device holds, so the heartbeat can report it.
	// Optional.
	observe func(*netcert.Material)
	log     *slog.Logger
}

func netcertLoop(ctx context.Context, cfg netcertConfig) error {
	// Whether we have already said the server is not issuing certificates.
	// Most deployments will never turn this on, and an agent that warns about
	// an unused feature twice a day forever teaches its operators that its
	// warnings are noise, which is expensive the day one of them matters.
	var reportedUnconfigured bool

	// attempt reports how long to wait before the next one.
	attempt := func() time.Duration {
		m, err := cfg.ensure(ctx)
		// m is whatever the device holds now, which on a failed renewal is the
		// certificate it had before. Published even on failure: the server
		// needs to know what this machine is ACTUALLY using, and a failed
		// renewal is exactly when that stops matching what we last issued.
		if cfg.observe != nil {
			cfg.observe(m)
		}

		phase := m.PhaseAt(time.Now(), cfg.agentID)
		var expires time.Time
		if m != nil {
			expires = m.NotAfter
		}

		switch {
		case err == nil:
		case errors.Is(err, netcert.ErrNotConfigured):
			if !reportedUnconfigured {
				cfg.log.Info("server is not issuing device certificates; 802.1X material will not be requested")
				reportedUnconfigured = true
			}
			// No CA on the server means there is no deadline to escalate
			// towards, so stay on the relaxed cadence and say nothing further.
			return cfg.every
		case errors.Is(err, context.Canceled):
			return cfg.every
		case phase == netcert.PhaseUrgent || phase == netcert.PhaseExpired:
			// Past the point where this is a routine retry. Renewal has been
			// failing for most of the margin and the machine is heading for,
			// or already in, an outage.
			cfg.log.Error("the network certificate is about to expire and cannot be renewed",
				"phase", phase.String(), "expires", expires.Format(time.RFC3339), "err", err)
			// The server cannot be told, because the broken thing IS the path
			// to the server: that is why renewal is failing. The person at the
			// machine is the only party still reachable, and the only one who
			// can plug in a cable or join the VPN.
			if cfg.warnUser != nil {
				cfg.warnUser(ctx, phase, expires)
			}
		default:
			// Ordinary. A failure here leaves whatever the device already held
			// untouched, so the machine keeps its network access, and there is
			// most of the margin still ahead.
			cfg.log.Warn("could not obtain a network certificate, will retry", "err", err)
		}

		if phase == netcert.PhaseUrgent || phase == netcert.PhaseExpired {
			return min(cfg.every, cfg.urgent)
		}
		return cfg.every
	}

	wait := attempt()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			timer.Reset(attempt())
		}
	}
}

// probeTimeout bounds one post-update health check. Every check it now makes
// is local state or a single round trip, so this is generous rather than tight.
const probeTimeout = 15 * time.Second

// Supervisor wires the modules to a shared NATS connection.
type Supervisor struct {
	NC       *nats.Conn
	AgentID  string
	Version  string
	StateDir string
	Log      *slog.Logger

	// Restart asks the process to exit so the service manager starts the
	// binary that self-update just swapped in. Without it the swap succeeds
	// and the old code keeps running, so self-update is refused instead.
	Restart func(reason string)

	// Handoff asks the process to exit WITHOUT asking to be restarted,
	// because something else is going to start it. Used by the Windows
	// update finalizer, which cannot swap the binary until this process
	// releases it.
	Handoff func(reason string)

	// Link reports asynchronous NATS errors. Optional; when nil the probe
	// simply cannot check for a silently denied subscription.
	Link *LinkHealth

	// RenewCredential asks the server for a fresh credential. Optional; when
	// nil the agent simply never renews, which is the pre-M4.5 behaviour.
	RenewCredential func(context.Context) error

	// RotateSecret persists a new agent secret handed down by the server.
	RotateSecret func(secret string) error

	// EnsureNetCert obtains or renews the device's 802.1X certificate and
	// reports whatever the device holds afterwards.
	//
	// The material comes back even on failure, and that is the point: how
	// urgent a failure is depends entirely on what is still on disk, and the
	// same error means "retry tomorrow" with three weeks left and "act now"
	// with hours left.
	//
	// Optional; when nil the agent never requests one, which is correct for
	// every deployment that does not use 802.1X.
	EnsureNetCert func(context.Context) (*netcert.Material, error)

	// WarnUserAboutCertificate interrupts whoever is at the machine when its
	// certificate is close to expiry and cannot be renewed. Optional.
	WarnUserAboutCertificate func(context.Context, netcert.Phase, time.Time)

	// Certificate carries what the device actually holds from the netcert
	// loop to the heartbeat. Created by Start when nil.
	Certificate *CertReport

	wg sync.WaitGroup
}

type task struct {
	name string
	run  func(context.Context, *slog.Logger) error
}

// Start builds the modules and launches every task. It returns immediately;
// cancel ctx to stop, then Wait for the goroutines to drain.
func (s *Supervisor) Start(ctx context.Context) {
	if s.Certificate == nil {
		s.Certificate = &CertReport{}
	}
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
		Update: jobs.UpdateDeps{
			StateDir: s.StateDir,
			Version:  s.Version,
			Runner:   runner,
			Audit:    aud,
			Log:      s.Log,
			Restart:  s.Restart,
			Handoff:  s.Handoff,
		},
		RotateSecret: s.RotateSecret,
	}
	scheduler := sched.New(s.AgentID, s.StateDir, jobsMod.RunScheduled, aud, s.Log)
	jobsMod.Sched = scheduler

	tasks := []task{
		{"heartbeat", func(ctx context.Context, log *slog.Logger) error {
			return heartbeat.Run(ctx, s.NC, s.AgentID, s.Version,
				scheduler.Version, s.Certificate.Get, log)
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
		// Credential renewal, PULLED on a timer rather than pushed by the
		// server. A rotation used to be delivered over NATS to a machine that
		// might be switched off, with a deadline on the old secret and nothing
		// retrying, so a laptop away for a long weekend came back holding a
		// credential that had already expired. An agent that asks cannot miss
		// the delivery.
		//
		// Runs once at startup, which is the case that matters: the machine
		// that was away is renewing within seconds of coming back.
		{"renew", func(ctx context.Context, log *slog.Logger) error {
			if s.RenewCredential == nil {
				return nil
			}
			return renewLoop(ctx, s.RenewCredential, log)
		}},
		// The 802.1X certificate, kept on its own timer for the same reason
		// credential renewal is pulled rather than pushed: the device that
		// most needs a fresh certificate is the one that has been switched
		// off, and it has to ask on its own once it is back.
		{"netcert", func(ctx context.Context, log *slog.Logger) error {
			if s.EnsureNetCert == nil {
				return nil
			}
			return netcertLoop(ctx, netcertConfig{
				ensure:   s.EnsureNetCert,
				agentID:  s.AgentID,
				every:    netcertCheckEvery,
				urgent:   netcertUrgentEvery,
				warnUser: s.WarnUserAboutCertificate,
				observe:  s.Certificate.Set,
				log:      log,
			})
		}},
		// Confirms a freshly updated build actually WORKS before the previous
		// one stops being the fallback. Getting this wrong is expensive in one
		// direction only: a probe that passes too easily clears the probation
		// marker and DELETES the rollback, so the fallback is gone precisely
		// when it was needed.
		//
		// It used to flush the connection and run a patch scan, described as
		// proving "the connection answers in both directions" and that "a real
		// unit of work completes end to end". Neither held. A flush is a
		// PING/PONG on the connection and says nothing about whether any
		// subscription is live; the patchstate publish is fire-and-forget core
		// NATS with no ack, so it returns success as soon as bytes reach the
		// local write buffer. Both are send-side checks, and the failure this
		// exists to catch is receive-side: a build whose jobs module cannot
		// bind, or whose subscription the server denied, publishes heartbeats
		// happily forever while being unable to accept a single job.
		//
		// The scan was also actively harmful here. A WUA search routinely takes
		// minutes inside one uninterruptible COM call, so a 30 second deadline
		// could never be met on Windows: the probe failed every time, retried
		// every 30 seconds for 24 hours, and queued another search onto the COM
		// thread on each attempt.
		{"update-health", func(ctx context.Context, log *slog.Logger) error {
			return update.Watch(ctx, update.WatchConfig{
				StateDir: s.StateDir,
				Log:      log,
				Probe: func(ctx context.Context) error {
					ctx, cancel := context.WithTimeout(ctx, probeTimeout)
					defer cancel()
					if !s.NC.IsConnected() {
						return errors.New("nats connection is not established")
					}
					if err := s.NC.FlushWithContext(ctx); err != nil {
						return fmt.Errorf("nats round trip: %w", err)
					}
					// The receive side, which is the whole point.
					if err := jobsMod.Ready(); err != nil {
						return fmt.Errorf("cannot accept jobs: %w", err)
					}
					if s.Link != nil {
						if err := s.Link.PermissionViolation(); err != nil {
							// Connected, publishing, and refused on a subject
							// it needs. Confirming this build would delete the
							// only way back from it.
							return fmt.Errorf("nats permissions violation: %w", err)
						}
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
