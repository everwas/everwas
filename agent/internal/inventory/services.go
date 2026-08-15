package inventory

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type svc struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type servicesSnapshot struct {
	Services []svc `json:"services"`
}

func collectServices(ctx context.Context) (any, error) {
	return servicesSnapshot{Services: listServices(ctx)}, nil
}

// listServices enumerates systemd services on Linux; other platforms report
// an empty list for now (M2).
func listServices(ctx context.Context) []svc {
	if runtime.GOOS != "linux" || !have("systemctl") {
		return []svc{}
	}
	out := run(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-pager", "--no-legend", "--plain")
	return parseSystemctlUnits(out)
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

// run executes a command and returns its stdout, or "" on any failure —
// inventory collection is always best-effort.
func run(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
