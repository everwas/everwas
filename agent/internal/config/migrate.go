package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// legacyDir is where an agent installed before the Everwas rename kept its
// state. Empty when there is nothing to migrate from on this platform.
//
// $EVERWAS_STATE_DIR deliberately has no legacy counterpart: an operator who
// points the state dir somewhere explicitly is telling us where it is, and
// second-guessing that by hunting for an older directory is how a dev machine
// picks up a stale identity from a previous experiment.
func legacyDir() string {
	if os.Getenv("EVERWAS_STATE_DIR") != "" {
		return ""
	}
	switch runtime.GOOS {
	case "linux":
		if os.Geteuid() == 0 {
			return "/etc/openrmm"
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".config", "openrmm")
	case "darwin":
		return "/Library/Application Support/OpenRMM"
	case "windows":
		return `C:\ProgramData\OpenRMM\Agent`
	}
	return ""
}

// MigrateLegacyState moves a pre-rename state directory to the current one.
//
// Without this, renaming the project silently un-enrols every machine that
// already had an agent. The new binary looks in the new directory, finds
// nothing, reports "not enrolled" and exits; the service then fails to start.
// The device cannot be recovered remotely either, because the credential it
// would authenticate with is precisely the thing it can no longer find, so the
// fix is a physical visit with a fresh enrollment token, per machine.
//
// The self-update path makes that fleet-wide and simultaneous: every agent
// downloads the new binary, swaps it, restarts into a build that cannot see its
// own identity, and goes dark together. That is the failure the whole update
// design exists to avoid, arriving through a rename rather than a bad build.
//
// Reports whether anything was moved, so the caller can say so once rather than
// leaving a fleet-wide migration unremarked in the logs.
func MigrateLegacyState() (migrated bool, err error) {
	legacy := legacyDir()
	if legacy == "" {
		return false, nil
	}
	current, err := Dir()
	if err != nil {
		return false, err
	}
	return migrateState(legacy, current)
}

// migrateState is the decision and the move, separated from resolving the two
// paths so it can be tested. The paths depend on the OS and the environment,
// which would otherwise make the interesting half of this untestable on the
// machine running the tests.
func migrateState(legacy, current string) (bool, error) {
	if legacy == "" || legacy == current {
		return false, nil
	}

	// The current directory winning is not merely "already migrated": it is
	// also the case where somebody enrolled fresh under the new name while an
	// old directory happens to still exist. Moving then would replace a working
	// identity with a stale one, so the newer location always wins and the old
	// directory is left alone for a human to remove.
	if _, err := os.Stat(filepath.Join(current, FileName)); err == nil {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(legacy, FileName)); err != nil {
		// Nothing to migrate. A fresh install, which is the common case and
		// must stay silent.
		return false, nil
	}

	// Rename the whole directory rather than copying agent.json alone. It also
	// holds the 802.1X key and certificate, the schedule cache, and the script
	// working area; leaving those behind would keep the machine enrolled while
	// silently dropping its network identity, which is a subtler version of the
	// same failure.
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		return false, fmt.Errorf("config: prepare %s: %w", current, err)
	}
	if err := os.Rename(legacy, current); err != nil {
		// Cross-device is the one failure worth naming, because /etc and a
		// state dir on another mount is a plausible layout and the error alone
		// ("invalid cross-device link") explains nothing about what to do.
		if errors.Is(err, os.ErrPermission) {
			return false, fmt.Errorf("config: move %s to %s: %w (run as root)", legacy, current, err)
		}
		return false, fmt.Errorf("config: move %s to %s: %w "+
			"(if these are on different filesystems, copy the directory by hand)",
			legacy, current, err)
	}
	return true, nil
}
