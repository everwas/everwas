package inventory

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rsp2k/openrmm/agent/internal/posture"
)

// postureSnapshot is the wire shape for the security posture kind.
//
// A list of per-check results rather than a rollup, deliberately. A single
// "compliant: true/false" cannot represent a machine where two checks passed,
// one failed and one could not run, and collapsing it here would force the
// collector to decide what an unassessable check means. That decision belongs
// to whoever carries the consequence of being wrong, which is not this code.
//
// Per-check also survives the checks themselves changing. The set will grow,
// and a machine assessed last month was assessed against last month's checks:
// per-check facts give a check added since no history before it existed, which
// is exactly right, where a rollup would silently restate the whole verdict.
type postureSnapshot struct {
	Checks []posture.Result `json:"checks"`
}

// collectPosture assesses this machine's security posture.
func collectPosture(ctx context.Context) (any, error) {
	checks := posture.Checks()
	if len(checks) == 0 {
		// A platform with no checks written yet, currently macOS. A platform
		// gap, not a fault: it must not publish an empty posture, because an
		// empty result set is indistinguishable from "everything was assessed
		// and nothing was found" to anything reading it later.
		return nil, fmt.Errorf("posture: %w", errNoCollector)
	}
	return postureSnapshot{Checks: posture.Run(ctx, checks, slog.Default())}, nil
}
