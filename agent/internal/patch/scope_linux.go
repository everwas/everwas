//go:build linux

package patch

import (
	"os"
	"os/exec"
)

// systemdMarker exists on any host whose PID 1 is systemd.
const systemdMarker = "/run/systemd/system"

// scopeCommand wraps a package manager in a transient systemd scope, so it
// runs in a cgroup of its own instead of the agent's.
//
// The agent's unit uses KillMode=mixed with a stop timeout. Without this
// wrapper, a `systemctl restart openrmm-agent` during a patch window gives
// dpkg that timeout and then an unsurvivable SIGKILL, leaving the package
// database for a human to repair. The self-inflicted version is worse: if
// the agent's own package is ever upgraded by a patch job, its postinst
// restarts the service, tearing down the cgroup that contains the dpkg
// calling it, simultaneously across a whole patch group.
//
// It reports false when the wrapper is unavailable, in which case the caller
// runs the package manager directly: no systemd, no root, or no systemd-run
// binary are all ordinary situations, not failures.
func scopeCommand(name string, args []string) (string, []string, bool) {
	if os.Geteuid() != 0 {
		return name, args, false
	}
	if _, err := os.Stat(systemdMarker); err != nil {
		return name, args, false
	}
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return name, args, false
	}
	// No --unit: systemd names the scope, so two concurrent transactions
	// cannot collide on it. --collect garbage collects a scope that failed.
	scoped := append([]string{
		"--scope",
		"--quiet",
		"--collect",
		"--description=OpenRMM patch transaction",
		"--",
		name,
	}, args...)
	return path, scoped, true
}
