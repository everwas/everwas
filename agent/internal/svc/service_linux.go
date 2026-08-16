//go:build linux

package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const systemctlTimeout = 90 * time.Second

// Install writes the systemd unit, reloads the daemon, then enables and
// starts the service. It is idempotent: running it over an existing install
// rewrites the unit and restarts the agent.
func Install(cfg InstallConfig) error {
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return err
	}
	// The guard goes down first: the unit's ExecStartPre points at it, and a
	// unit that references a guard which is not there yet is a unit that
	// starts without its rollback path.
	if err := writeGuard(cfg.Prefix); err != nil {
		return fmt.Errorf("svc: write %s: %w", GuardPath(cfg.Prefix), err)
	}
	unit := UnitPath(cfg.Prefix)
	if err := writeFileAtomic(unit, []byte(RenderSystemdUnit(cfg)), 0o644); err != nil {
		return fmt.Errorf("svc: write %s: %w", unit, err)
	}
	if cfg.Prefix != "" {
		// A relocated install is a dry run. Never talk to the real systemd.
		return nil
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", Name); err != nil {
		return err
	}
	return nil
}

// Uninstall stops and disables the service and removes the unit file. A
// service that was never installed is not an error: the end state is the same.
func Uninstall() error {
	prefix := prefixOrEnv()
	unit := UnitPath(prefix)
	if prefix == "" {
		// Best effort: a masked or already removed unit must not stop us from
		// deleting the file.
		_ = systemctl("disable", "--now", Name)
	}
	if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("svc: remove %s: %w", unit, err)
	}
	if err := removeGuard(prefix); err != nil {
		return fmt.Errorf("svc: remove %s: %w", GuardPath(prefix), err)
	}
	if prefix == "" {
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		_ = systemctl("reset-failed", Name)
	}
	return nil
}

// Status reports running, stopped or not installed.
func Status() (string, error) {
	if _, err := os.Stat(UnitPath(prefixOrEnv())); os.IsNotExist(err) {
		return StatusNotInstalled, nil
	}
	out, err := systemctlOutput("is-active", Name)
	state := strings.TrimSpace(out)
	switch state {
	case "active", "activating", "reloading":
		return StatusRunning, nil
	case "inactive", "failed", "deactivating":
		return StatusStopped, nil
	case "":
		if err != nil {
			return StatusUnknown, err
		}
		return StatusUnknown, nil
	default:
		return state, nil
	}
}

// Start starts the service.
func Start() error { return systemctl("start", Name) }

// Stop stops the service.
func Stop() error { return systemctl("stop", Name) }

// Restart restarts the service.
func Restart() error { return systemctl("restart", Name) }

func systemctl(args ...string) error {
	out, err := systemctlOutput(args...)
	if err != nil {
		return fmt.Errorf("svc: systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func systemctlOutput(args ...string) (string, error) {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return "", errors.New("svc: systemctl not found; this host does not use systemd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return string(out), err
}
