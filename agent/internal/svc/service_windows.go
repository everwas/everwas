//go:build windows

package svc

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// recoveryResetPeriod is how long the SCM waits with no failures before it
// forgets the failure count. One day matches what most agents ship with.
const recoveryResetPeriod = uint32(24 * 60 * 60)

// Install registers the agent with the Service Control Manager, sets it to
// start automatically, and configures recovery: restart at 5s and 30s, then a
// command at 60s that restores the previous binary.
// An existing service is updated in place rather than recreated so its SID
// and any operator ACL changes survive an upgrade.
func Install(cfg InstallConfig) error {
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.Prefix != "" {
		// A relocated install is a dry run. Never talk to the real SCM.
		return nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("svc: connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	conf := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  DisplayName,
		Description:  Description,
	}

	s, err := m.OpenService(Name)
	if err == nil {
		defer s.Close()
		existing, cErr := s.Config()
		if cErr != nil {
			return fmt.Errorf("svc: read existing service config: %w", cErr)
		}
		existing.StartType = conf.StartType
		existing.ErrorControl = conf.ErrorControl
		existing.DisplayName = conf.DisplayName
		existing.Description = conf.Description
		existing.BinaryPathName = binaryPathName(cfg)
		if err := s.UpdateConfig(existing); err != nil {
			return fmt.Errorf("svc: update service config: %w", err)
		}
	} else {
		s, err = m.CreateService(Name, cfg.BinaryPath, conf, cfg.Args...)
		if err != nil {
			return fmt.Errorf("svc: create service: %w", err)
		}
		defer s.Close()
	}

	// Restart on every failure, with backoff. Deliberately no RunCommand.
	//
	// The third action used to be `move /Y <target>.old <target> && sc start`,
	// intended as the Windows stand-in for the unix ExecStartPre guard. It was
	// not one. The unix guard reads the probation file, checks the version
	// under test, and records a denial; this command did none of that. It fired
	// on ANY three failures inside recoveryResetPeriod, whatever the cause,
	// then moved an arbitrarily old backup over the current binary. Backups are
	// never retired, so on a host running a version from two months ago that is
	// a silent two-month downgrade, and because it MOVES rather than copies,
	// the only backup a real rollback could have used is gone afterwards. No
	// denylist entry, no state file change, nothing in the audit trail.
	//
	// Three unrelated failures in 24 hours is not a rare shape: a revoked NATS
	// credential produces exactly that, and so does anything else that makes
	// the agent exit non-zero on a schedule. It became likelier still once a
	// deliberate restart-after-update started exiting non-zero, which it must
	// (see exitRestart) for the SCM to restart the agent at all.
	//
	// What this leaves uncovered is a Windows binary that cannot execute at
	// all, where the in-process rollback cannot run either. That is a real gap
	// and narrower than what the command cost: repeated restarts are always
	// safe, a blind downgrade is not.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, recoveryResetPeriod); err != nil {
		return fmt.Errorf("svc: set recovery actions: %w", err)
	}
	// Clear the command a previous version registered. No action references it
	// any more, so it is inert, but a host upgraded from an older agent would
	// otherwise keep the downgrade one-liner sitting in its service config
	// indefinitely, waiting for anyone who adds a RunCommand action back.
	if err := s.SetRecoveryCommand(""); err != nil {
		return fmt.Errorf("svc: clear recovery command: %w", err)
	}
	// The agent exits non-zero for both of its abnormal reasons (a restart
	// after a self-update, and a connection it cannot recover), and neither is
	// a crash in the SCM's sense: the process terminated normally. Without
	// this, neither gets a restart, and a self-update leaves the host with no
	// agent process until the next reboot.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("svc: set recovery on non-crash exit: %w", err)
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("svc: start service: %w", err)
	}
	return nil
}

// binaryPathName renders the quoted "exe args" string the SCM stores.
func binaryPathName(cfg InstallConfig) string {
	out := windows.EscapeArg(cfg.BinaryPath)
	for _, a := range cfg.Args {
		out += " " + windows.EscapeArg(a)
	}
	return out
}

// Uninstall stops the service and deletes it. A missing service is not an
// error.
func Uninstall() error {
	if prefixOrEnv() != "" {
		return nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("svc: connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(Name)
	if err != nil {
		return nil // not installed
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil &&
		!errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) &&
		!errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) {
		return fmt.Errorf("svc: stop service: %w", err)
	}
	waitForStop(s, 30*time.Second)

	if err := s.Delete(); err != nil {
		return fmt.Errorf("svc: delete service: %w", err)
	}
	return nil
}

// Status reports running, stopped or not installed.
func Status() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return StatusUnknown, fmt.Errorf("svc: connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(Name)
	if err != nil {
		return StatusNotInstalled, nil
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return StatusUnknown, fmt.Errorf("svc: query service: %w", err)
	}
	switch st.State {
	case svc.Running, svc.StartPending, svc.ContinuePending:
		return StatusRunning, nil
	case svc.Stopped, svc.StopPending, svc.Paused, svc.PausePending:
		return StatusStopped, nil
	default:
		return StatusUnknown, nil
	}
}

// Start starts the service.
func Start() error { return control(func(s *mgr.Service) error { return s.Start() }) }

// Stop stops the service and waits for it to settle.
func Stop() error {
	return control(func(s *mgr.Service) error {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
		waitForStop(s, 30*time.Second)
		return nil
	})
}

// Restart stops then starts the service.
func Restart() error {
	if err := Stop(); err != nil {
		return err
	}
	return Start()
}

func control(fn func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("svc: connect to service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return ErrNotInstalled
	}
	defer s.Close()
	return fn(s)
}

func waitForStop(s *mgr.Service, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}
