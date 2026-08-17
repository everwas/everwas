package cli

import (
	"context"
	"testing"
)

// A deliberate restart must exit NON-ZERO.
//
// The Windows SCM decides whether to run its recovery actions from the exit
// code: SetRecoveryActionsOnNonCrashFailures makes a non-zero exit count as a
// failure worth acting on, and exit 0 is "the service finished, leave it
// stopped". StartType=Automatic only applies at boot. So a self-update that
// exits 0 leaves the host with no agent process until somebody reboots it,
// fleet-wide, the moment an update is accepted.
//
// The previous code returned 0 here, justified by "exiting non-zero would make
// the rollback tracker count a crash". It would not: both crash counters count
// process STARTS inside a window (Tracker.RecordStart, countStarts, and the
// unix guard's own start file). Nothing anywhere reads an exit code, so the
// non-zero exit costs nothing and buys the restart.
func TestADeliberateRestartExitsNonZero(t *testing.T) {
	if exitRestart == 0 {
		t.Fatal("a restart that exits 0 is a stop: the SCM will not start it again")
	}
}

func TestShutdownReasonsMapToDistinctExitCodes(t *testing.T) {
	cases := []struct {
		name string
		why  stopReason
		want int
	}{
		// Asked to stop: the only success. Anything else must tell the service
		// manager to act.
		{"asked to stop", stopReason{}, 0},
		{"restart after update", stopReason{restart: "updated to X"}, exitRestart},
		{"deaf connection", stopReason{deaf: true}, exitDeaf},
	}
	for _, tc := range cases {
		if got := exitCodeFor(tc.why); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRestartIsStillNotReportedAsADeafConnection(t *testing.T) {
	// Kept from the original test: the two non-zero reasons must stay
	// distinguishable, because one means "start me again" and the other means
	// "something is wrong with my connection".
	restart := make(chan stopReason, 1)
	restart <- stopReason{restart: "updated to 2026.8.16"}

	why := waitForShutdown(context.Background(), make(chan struct{}), restart)
	if why.deaf {
		t.Fatal("a self-update restart was misreported as a deaf connection")
	}
	if why.restart == "" {
		t.Fatal("the restart reason was lost")
	}
}
