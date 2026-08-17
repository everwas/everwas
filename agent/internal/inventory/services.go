package inventory

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// runner enumerates something by running a command. Injected rather than
// called directly so the failure paths below are testable: the bug this
// package had was entirely in what happens when the command does not work,
// which is the one case a test against the real host cannot arrange.
type runner func(ctx context.Context, name string, args ...string) (string, error)

// errNoCollector means this platform has no implementation for a kind.
//
// A third state, distinct from both "the machine has none" and "we could not
// look". It must not publish, because reporting zero services on a Windows box
// is a false claim about the machine, and it must not be logged as a failure
// either, or every macOS agent warns forever about a gap it cannot fix.
var errNoCollector = errors.New("no collector for this platform")

type svc struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type servicesSnapshot struct {
	Services []svc `json:"services"`
}

func collectServices(ctx context.Context) (any, error) {
	return collectServicesWith(ctx, run)
}

// collectServicesWith enumerates systemd services on Linux.
//
// Returns an error rather than an empty list when the enumeration fails. An
// empty list is a claim that this host runs no services, and the server
// records anything missing from a snapshot as removed, so a systemctl that
// times out under I/O pressure would retire every service on the host and then
// re-add them all on the next poll: a fabricated change event on a machine
// where nothing changed.
func collectServicesWith(ctx context.Context, exec runner) (any, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("services: %w", errNoCollector)
	}
	if !have("systemctl") {
		// No systemd is a real answer about the host, not a failure to look.
		return servicesSnapshot{Services: []svc{}}, nil
	}
	out, err := exec(ctx, "systemctl",
		"list-units", "--type=service", "--all", "--no-pager", "--no-legend", "--plain")
	if err != nil {
		return nil, fmt.Errorf("services: systemctl list-units: %w", err)
	}
	return servicesSnapshot{Services: parseSystemctlUnits(out)}, nil
}

// parseSystemctlUnits parses `--no-legend --plain` output: columns are
// UNIT LOAD ACTIVE SUB DESCRIPTION; we keep the unit name and ACTIVE state.
func parseSystemctlUnits(out string) []svc {
	services := []svc{}
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		services = append(services, svc{Name: fields[0], State: fields[2]})
	}
	return services
}

// have reports whether a binary is on PATH.
func have(binary string) bool {
	_, err := exec.LookPath(binary)
	return err == nil
}

// run executes a command and returns its stdout.
//
// It used to swallow every error and return "", described as inventory being
// "always best-effort". That is where this package's worst bug lived: every
// caller parsed "" into an empty list and published it as fact, so a
// dpkg-query that lost a race for the frontend lock reported a host with no
// software installed. Best-effort is the right instinct for one collector
// failing; it is the wrong instinct for what that collector then claims.
func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
