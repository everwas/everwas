package patch

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// softwareUpdateManager drives macOS `softwareupdate`.
type softwareUpdateManager struct {
	loggerHolder
	gate installGate

	mu sync.Mutex
	// rebootPending records that an install in this agent's lifetime asked
	// for a restart. macOS exposes no system-wide "reboot required" flag the
	// way Debian and Windows do, so this is the only honest signal we have;
	// it resets when the agent restarts, which after a reboot is correct.
	rebootPending bool
}

func (m *softwareUpdateManager) Kind() string { return BackendSoftwareUpdate }

// Scan lists available updates. Blocks that will not parse are logged and
// skipped so a format change in a future macOS costs us one entry, not the
// whole snapshot.
func (m *softwareUpdateManager) Scan(ctx context.Context) ([]Update, error) {
	res := runCmd(ctx, execOptions{Timeout: scanTimeout}, "softwareupdate", "--list")
	if res.Err != nil {
		return nil, fmt.Errorf("softwareupdate --list: %w", res.Err)
	}
	// softwareupdate writes its listing to stderr on several macOS
	// releases and to stdout on others, so both are parsed.
	updates, degraded := parseSoftwareUpdateList(res.Stdout+"\n"+res.Stderr, m.host(ctx))
	if len(degraded) > 0 {
		m.logger().Warn("softwareupdate blocks skipped, scan degraded",
			"count", len(degraded), "labels", strings.Join(degraded, "; "))
	}
	if len(updates) == 0 && res.ExitCode != 0 && !strings.Contains(res.combined(), "No new software") {
		return nil, fmt.Errorf("softwareupdate --list exited %d: %s", res.ExitCode, res.tail(5))
	}
	return updates, nil
}

// host describes the machine so the parser can decide what is installable.
func (m *softwareUpdateManager) host(ctx context.Context) macOSHost {
	h := macOSHost{AppleSilicon: runtime.GOARCH == "arm64"}
	res := runCmd(ctx, execOptions{Timeout: quickTimeout}, "sw_vers", "-productVersion")
	if res.ok() {
		h.MajorVersion = parseSWVersMajor(res.Stdout)
	}
	if h.MajorVersion == 0 {
		m.logger().Warn("could not read macOS version, OS updates will be reported unsupported")
	}
	return h
}

// Install applies the named labels one at a time so a failure names the
// update that caused it. --no-restart is not optional: the agent reports
// that a reboot is needed and lets the operator schedule it.
//
// Updates the scan marked Unsupported are refused here rather than
// attempted: on Apple Silicon softwareupdate would either prompt for
// credentials that no one is there to type or half-stage an update.
func (m *softwareUpdateManager) Install(ctx context.Context, ids []string, progress func(InstallProgress)) (InstallResult, error) {
	ids = dedupeIDs(ids)
	res := newInstallResult()
	if len(ids) == 0 {
		return res, nil
	}
	if !m.gate.acquire() {
		return res, ErrBusy
	}
	defer m.gate.release()

	available, err := m.Scan(ctx)
	if err != nil {
		return res, err
	}
	byID := make(map[string]Update, len(available))
	for _, u := range available {
		byID[u.ID] = u
	}

	restartNeeded := false
	for i, id := range ids {
		u, known := byID[id]
		switch {
		case !known:
			res.fail(id, errors.New("update is no longer offered by softwareupdate"))
			continue
		case u.Unsupported:
			res.fail(id, errors.New(u.Detail))
			continue
		}

		emitProgress(progress, InstallProgress{
			UpdateID: id, Phase: PhaseDownload, Pct: pctOf(i, len(ids)),
		})
		out := runCmd(ctx, execOptions{
			Timeout: installTimeout,
			OnLine: func(string) {
				emitProgress(progress, InstallProgress{
					UpdateID: id, Phase: PhaseInstall, Pct: pctOf(i, len(ids)),
				})
			},
		}, "softwareupdate", "--install", id, "--no-restart")

		switch {
		case out.Err != nil:
			res.fail(id, fmt.Errorf("softwareupdate --install: %w", out.Err))
		case out.ExitCode != 0:
			res.fail(id, fmt.Errorf("softwareupdate --install exited %d: %s",
				out.ExitCode, out.tail(5)))
		case softwareUpdateInstallFailed(out.combined()):
			// softwareupdate exits 0 on some failures, so the text decides.
			res.fail(id, errors.New(out.tail(3)))
		default:
			res.Installed = append(res.Installed, id)
			restartNeeded = restartNeeded || u.RebootLikely
		}
	}

	if restartNeeded {
		m.mu.Lock()
		m.rebootPending = true
		m.mu.Unlock()
	}
	res.RebootRequired = restartNeeded
	return res, nil
}

// RebootRequired reports whether an install this agent ran asked for a
// restart. macOS has no equivalent of /var/run/reboot-required, so there is
// nothing to read for updates applied before the agent started.
func (m *softwareUpdateManager) RebootRequired(_ context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rebootPending, nil
}
