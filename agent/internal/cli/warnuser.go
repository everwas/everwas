package cli

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/netcert"
	"github.com/rsp2k/openrmm/agent/internal/notify"
)

// tellUserEvery bounds how often the person at the machine is interrupted.
//
// Once a day. Often enough that a deadline several days out is not missed,
// rare enough that it does not become the dialog they dismiss without reading,
// which would waste the only channel that still works when the server is
// unreachable.
const tellUserEvery = 24 * time.Hour

// warnUserAboutCertificate builds the callback that interrupts whoever is
// using this machine when its certificate is close to expiry.
//
// The last-warned time is kept on DISK rather than in memory. An agent that is
// crash-looping, or a machine being rebooted repeatedly by a frustrated user,
// would otherwise reset the timer on every start and pop a dialog every few
// seconds, which is how a warning becomes something people click away by
// reflex.
func warnUserAboutCertificate(stateDir string, log *slog.Logger) func(context.Context, netcert.Phase, time.Time) {
	stamp := filepath.Join(stateDir, "certificate-warning-shown")

	return func(ctx context.Context, phase netcert.Phase, expires time.Time) {
		if info, err := os.Stat(stamp); err == nil {
			if time.Since(info.ModTime()) < tellUserEvery {
				return
			}
		}

		title, body := certificateWarning(phase, expires, time.Now())
		switch err := notify.Local(ctx, title, body); {
		case err == nil:
			log.Info("warned the user that the network certificate is expiring",
				"expires", expires.Format(time.RFC3339))
		default:
			// Nobody logged in, a headless server, a desktop we cannot reach.
			// Ordinary, and never a reason to stop trying to renew.
			log.Debug("could not warn the user about the certificate", "err", err)
		}

		// Stamped whether or not anyone was there to see it. Retrying every
		// hour on an unattended machine would achieve nothing except a dialog
		// storm the moment somebody finally logs in.
		if err := os.WriteFile(stamp, nil, 0o600); err != nil {
			log.Debug("could not record that the user was warned", "err", err)
		} else if err := os.Chtimes(stamp, time.Now(), time.Now()); err != nil {
			log.Debug("could not stamp the certificate warning", "err", err)
		}
	}
}

// certificateWarning writes what a person, not an operator, needs to read.
//
// No serial, no path, no phase name. Someone whose laptop is about to fall off
// the network needs to know what will happen, roughly when, and the one thing
// they can do about it.
func certificateWarning(phase netcert.Phase, expires time.Time, now time.Time) (title, body string) {
	if phase == netcert.PhaseExpired {
		return "Network access needs attention",
			"This computer's network certificate has expired, so it may lose access to " +
				"the company network. Connect it to the office network or the VPN and " +
				"leave it on for a few minutes so it can renew itself."
	}

	left := expires.Sub(now)
	return "Network access expires " + humanDeadline(left),
		fmt.Sprintf("This computer's network certificate expires %s and it has not been "+
			"able to renew. Connect it to the office network or the VPN and leave it on "+
			"for a few minutes. If it expires, this computer may lose network access.",
			humanDeadline(left))
}

// humanDeadline says "tomorrow", not "in 23h47m".
func humanDeadline(d time.Duration) string {
	switch days := int(math.Floor(d.Hours() / 24)); {
	case d <= 0:
		return "today"
	case days == 0:
		return "today"
	case days == 1:
		return "tomorrow"
	default:
		return fmt.Sprintf("in %d days", days)
	}
}
