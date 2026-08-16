//go:build darwin

package svc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const launchctlTimeout = 90 * time.Second

// serviceTarget is the modern launchctl domain target for a system daemon.
func serviceTarget() string { return "system/" + LaunchdLabel }

// Install writes the launchd plist and bootstraps it into the system domain.
// Bootstrap is the supported path on Yosemite and later; the older
// "load -w" verb is kept as a fallback because it still works everywhere and
// costs one extra exec on the rare host where bootstrap is unavailable.
func Install(cfg InstallConfig) error {
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return err
	}
	plist := PlistPath(cfg.Prefix)
	if err := os.MkdirAll(filepath.Join(cfg.Prefix, MacLogDir), 0o755); err != nil {
		return fmt.Errorf("svc: create log dir: %w", err)
	}
	if err := writeFileAtomic(plist, []byte(RenderLaunchdPlist(cfg)), 0o644); err != nil {
		return fmt.Errorf("svc: write %s: %w", plist, err)
	}
	if cfg.Prefix != "" {
		// A relocated install is a dry run. Never talk to the real launchd.
		return nil
	}
	// Remove any previous registration so a rewritten plist actually takes.
	_ = launchctl("bootout", serviceTarget())
	if err := launchctl("bootstrap", "system", plist); err != nil {
		if fallbackErr := launchctl("load", "-w", plist); fallbackErr != nil {
			return fmt.Errorf("%w (load -w fallback: %v)", err, fallbackErr)
		}
		return nil
	}
	return launchctl("kickstart", "-k", serviceTarget())
}

// Uninstall boots the daemon out of the system domain and removes the plist.
func Uninstall() error {
	prefix := prefixOrEnv()
	plist := PlistPath(prefix)
	if prefix == "" {
		if err := launchctl("bootout", serviceTarget()); err != nil {
			_ = launchctl("unload", "-w", plist)
		}
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("svc: remove %s: %w", plist, err)
	}
	return nil
}

// Status reports running, stopped or not installed. launchctl print exits
// non-zero when the label is not loaded, which is how "stopped" is detected.
func Status() (string, error) {
	if _, err := os.Stat(PlistPath(prefixOrEnv())); os.IsNotExist(err) {
		return StatusNotInstalled, nil
	}
	out, err := launchctlOutput("print", serviceTarget())
	if err != nil {
		return StatusStopped, nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			if strings.Contains(line, "running") {
				return StatusRunning, nil
			}
			return StatusStopped, nil
		}
	}
	if strings.Contains(out, "pid = ") {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

// Start starts the daemon.
func Start() error { return launchctl("kickstart", serviceTarget()) }

// Stop stops the daemon without unloading it.
func Stop() error { return launchctl("kill", "SIGTERM", serviceTarget()) }

// Restart restarts the daemon.
func Restart() error { return launchctl("kickstart", "-k", serviceTarget()) }

func launchctl(args ...string) error {
	out, err := launchctlOutput(args...)
	if err != nil {
		return fmt.Errorf("svc: launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func launchctlOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	return string(out), err
}
