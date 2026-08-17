package svc

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
)

// startupGrace is how long we tell the SCM to keep waiting while the agent
// comes up. The SCM's own patience is 30 s by default and it kills anything
// that has not reported Running by then.
const startupGrace = 20 * time.Second

// IsService reports whether the SCM started this process.
func IsService() (bool, error) { return svc.IsWindowsService() }

// RunAsService runs the agent under the Windows service control dispatcher.
//
// This is not optional plumbing. A Windows service binary MUST call
// StartServiceCtrlDispatcher and report SERVICE_RUNNING, and the agent did
// neither: `install` registered the service correctly and starting it always
// failed with error 1053, "the service did not respond to the start or
// control request in a timely fashion", after a 30 second stall.
//
// Nothing catches this anywhere else. On Linux systemd runs the binary as an
// ordinary process and reads its exit code, so `run` was completely correct
// there, and the Windows unit tests never start a real service. It only
// appears when the SCM actually starts the thing.
func RunAsService(name string, work func(context.Context) int) int {
	h := &handler{work: work}
	if err := svc.Run(name, h); err != nil {
		return 1
	}
	return h.code
}

type handler struct {
	work func(context.Context) int
	code int
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	// StartPending first with a hint, so a slow start reads as "still coming
	// up" rather than "hung". The agent dials NATS during startup and that can
	// be slow on a machine whose network is not ready yet.
	s <- svc.Status{State: svc.StartPending, WaitHint: uint32(startupGrace / time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- h.work(ctx) }()

	// Accept Shutdown as well as Stop. Without it the agent is killed outright
	// when the machine reboots, so in-flight jobs never reach a terminal state
	// and the server shows them running for ever.
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case h.code = <-done:
			// The agent exited on its own: a closed NATS connection, or a
			// self-update asking to be restarted. Report Stopped and let the
			// SCM's recovery actions restart us.
			s <- svc.Status{State: svc.Stopped}
			return false, uint32(h.code)

		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus

			case svc.Stop, svc.Shutdown:
				// Say StopPending BEFORE cancelling. A service that goes quiet
				// without it is killed, and in-flight jobs then never reach a
				// terminal state so the server shows them running for ever.
				h.code = h.awaitStop(done, s, cancel)
				s <- svc.Status{State: svc.Stopped}
				return false, uint32(h.code)
			}
		}
	}
}

// stopGrace bounds the wait for modules to finish shutting down.
//
// Bounded on purpose. A service stuck in StopPending blocks Windows shutdown
// and needs a reboot to clear, which is far worse than losing the tail of one
// job's cleanup.
const stopGrace = 45 * time.Second

// checkpointEvery is how often we re-report StopPending while draining.
//
// WaitHint is not "how long we will take", it is "how long before the next
// status update". Announcing the whole grace period once and going quiet is
// what makes services.msc call a service hung, so we tick instead: each update
// carries an incrementing CheckPoint, which is the SCM's proof of progress.
const checkpointEvery = 5 * time.Second

// awaitStop announces the stop, triggers it, and drains with the SCM kept
// informed. It reports the agent's exit code.
func (h *handler) awaitStop(done <-chan int, s chan<- svc.Status, cancel context.CancelFunc) int {
	deadline := time.NewTimer(stopGrace)
	defer deadline.Stop()
	tick := time.NewTicker(checkpointEvery)
	defer tick.Stop()

	checkpoint := uint32(1)
	report := func() {
		s <- svc.Status{
			State:      svc.StopPending,
			CheckPoint: checkpoint,
			// Slack on the hint: a status update that lands a moment late
			// must not read as a missed deadline.
			WaitHint: uint32(2 * checkpointEvery / time.Millisecond),
		}
		checkpoint++
	}
	report()
	cancel()

	for {
		select {
		case code := <-done:
			return code
		case <-tick.C:
			report()
		case <-deadline.C:
			// Drain overran. Stopping anyway beats hanging the machine.
			return 1
		}
	}
}
