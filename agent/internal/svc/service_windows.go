//go:build windows

package svc

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/rsp2k/openrmm/agent/internal/update"
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

	// The third action is the Windows stand-in for the unix ExecStartPre
	// guard: two restarts, then a command that puts the previous binary back.
	// It runs from the SCM, not from the agent, which is what makes it work
	// when the new build cannot execute at all.
	if err := s.SetRecoveryCommand(recoveryCommand(cfg)); err != nil {
		return fmt.Errorf("svc: set recovery command: %w", err)
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.RunCommand, Delay: 60 * time.Second},
	}, recoveryResetPeriod); err != nil {
		return fmt.Errorf("svc: set recovery actions: %w", err)
	}
	// A clean exit(0) after a self-update is not a crash as far as the SCM is
	// concerned, so ask it to run the recovery actions anyway. That is what
	// restarts the agent into the new binary.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("svc: set recovery on non-crash exit: %w", err)
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("svc: start service: %w", err)
	}
	return nil
}

// recoveryCommand restores the previous binary and starts the service again.
// A failed move (there is no backup) leaves the start out, because there is
// nothing to start into.
//
// It is a cmd.exe one liner rather than a call back into the agent on
// purpose: the agent is the thing that could not run.
func recoveryCommand(cfg InstallConfig) string {
	target := cfg.BinaryPath
	backup := update.BackupPath(target)
	return fmt.Sprintf(`cmd.exe /C move /Y "%s" "%s" && sc.exe start %s`, backup, target, Name)
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
