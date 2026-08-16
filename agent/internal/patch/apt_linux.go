package patch

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// aptUpdateAttempts is how many times `apt-get update` is retried on a
	// transient repository error before we give up and use whatever is
	// already in the cache.
	aptUpdateAttempts = 3

	// aptLockBudget bounds how long an install waits out dpkg lock
	// contention. Ten minutes covers an unattended-upgrades run; past that
	// something is stuck and the operator needs to know.
	aptLockBudget = 10 * time.Minute

	// rebootRequiredFlag is dropped by update-notifier-common and by kernel
	// and libc postinst scripts on Debian and Ubuntu.
	rebootRequiredFlag = "/var/run/reboot-required"
)

// aptManager drives apt-get on Debian and Ubuntu.
type aptManager struct {
	loggerHolder
	gate installGate
}

func (m *aptManager) Kind() string { return BackendAPT }

// Scan refreshes the package lists and then asks apt what a full upgrade
// would do. A refresh that fails is logged and tolerated: a stale cache
// still reports real pending updates, and reporting yesterday's list beats
// reporting nothing because one mirror was down.
func (m *aptManager) Scan(ctx context.Context) ([]Update, error) {
	if err := m.refresh(ctx); err != nil {
		m.logger().Warn("apt update failed, scanning against the existing cache", "err", err)
	}
	res := runCmd(ctx, execOptions{Timeout: scanTimeout},
		"apt-get", "-s", "-o", "Debug::NoLocking=1", "dist-upgrade")
	if res.Err != nil {
		return nil, fmt.Errorf("apt-get dist-upgrade simulate: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("apt-get dist-upgrade simulate exited %d: %s",
			res.ExitCode, res.tail(5))
	}
	return parseAptSimulate(res.Stdout), nil
}

// refresh runs `apt-get update`, retrying only errors that look transient.
func (m *aptManager) refresh(ctx context.Context) error {
	var last error
	backoff := 3 * time.Second
	for attempt := 1; attempt <= aptUpdateAttempts; attempt++ {
		res := runCmd(ctx, execOptions{Timeout: scanTimeout}, "apt-get", "update")
		if res.ok() {
			return nil
		}
		if res.Err != nil {
			last = res.Err
		} else {
			last = fmt.Errorf("apt-get update exited %d: %s", res.ExitCode, res.tail(5))
		}
		if !aptIsTransientUpdateError(res.combined()) {
			return last
		}
		if attempt == aptUpdateAttempts {
			break
		}
		m.logger().Debug("apt update transient failure, retrying",
			"attempt", attempt, "backoff", backoff.String())
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff *= 2
	}
	return last
}

// Install upgrades the named packages to the exact versions in ids. The
// dpkg options keep existing config files without prompting: an RMM agent
// must never leave a host sitting at a conffile question.
func (m *aptManager) Install(ctx context.Context, ids []string, progress func(InstallProgress)) (InstallResult, error) {
	ids = dedupeIDs(ids)
	res := newInstallResult()
	if len(ids) == 0 {
		return res, nil
	}
	if !m.gate.acquire() {
		return res, ErrBusy
	}
	defer m.gate.release()

	// Scan first and install only what apt is actually offering. It costs a
	// refresh on every install, which is the price of never handing apt a
	// package name that nobody approved.
	available, err := m.Scan(ctx)
	if err != nil {
		return res, fmt.Errorf("apt scan before install: %w", err)
	}

	specs, want, idByName := aptInstallPlan(ids, offeredIDs(available), &res)
	if len(specs) == 0 {
		return res, nil
	}

	// "--" ends option parsing. Without it an id whose name half begins with
	// a dash reaches apt as an option: "--option=Dpkg::Options::=--force-confnew"
	// round-tripped through the id and inverted the conffile safety set on
	// the two lines above it.
	args := append([]string{
		"-y",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"install",
		"--",
	}, specs...)

	cmdRes, err := m.runInstall(ctx, args, len(specs), progress)
	if err != nil {
		for _, id := range idByName {
			res.fail(id, err)
		}
		res.RebootRequired = fileExists(rebootRequiredFlag)
		return res, err
	}

	// Verify against dpkg rather than trusting the exit code: apt can
	// succeed overall while a single package was held back, and the
	// operator needs per-update truth.
	installed := m.installedVersions(ctx, want)
	for name, version := range want {
		id := idByName[name]
		switch got, ok := installed[name]; {
		case ok && got == version:
			res.Installed = append(res.Installed, id)
		case ok:
			res.fail(id, fmt.Errorf("installed version is %s, wanted %s", got, version))
		default:
			res.fail(id, errors.New("package not installed after apt-get install: "+cmdRes.tail(3)))
		}
	}
	res.RebootRequired = fileExists(rebootRequiredFlag)
	return res, nil
}

// runInstall executes apt-get, waiting out dpkg lock contention with
// backoff. An install that cannot get the lock inside the budget fails
// loudly; silently skipping the run would report a patched host that is not.
func (m *aptManager) runInstall(ctx context.Context, args []string, total int, progress func(InstallProgress)) (cmdResult, error) {
	deadline := time.Now().Add(aptLockBudget)
	backoff := 10 * time.Second
	for {
		seen := 0
		res := runCmd(ctx, execOptions{
			Timeout: installTimeout,
			OnLine: func(line string) {
				phase, ok := aptProgressPhase(line)
				if !ok {
					return
				}
				if phase == PhaseInstall {
					seen++
				}
				emitProgress(progress, InstallProgress{
					Phase: phase,
					Pct:   pctOf(seen, total),
				})
			},
		}, "apt-get", args...)

		if res.ok() {
			return res, nil
		}
		if res.Err != nil {
			return res, fmt.Errorf("apt-get install: %w", res.Err)
		}
		if !aptIsLockContention(res.combined()) {
			return res, fmt.Errorf("apt-get install exited %d: %s", res.ExitCode, res.tail(8))
		}
		if time.Now().After(deadline) {
			return res, fmt.Errorf("apt-get install: dpkg lock still held after %s: %s",
				aptLockBudget, res.tail(3))
		}
		m.logger().Info("dpkg lock held, waiting", "backoff", backoff.String())
		if !sleepCtx(ctx, backoff) {
			return res, ctx.Err()
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// installedVersions asks dpkg what is on disk for the packages we touched.
func (m *aptManager) installedVersions(ctx context.Context, want map[string]string) map[string]string {
	args := []string{"-W", "-f", "${Package}\t${Version}\n"}
	for name := range want {
		args = append(args, name)
	}
	res := runCmd(ctx, execOptions{Timeout: quickTimeout}, "dpkg-query", args...)
	// dpkg-query exits 1 when any named package is unknown, but still
	// prints the ones it does know, so the output is used regardless.
	return parseDpkgVersions(res.Stdout)
}

// RebootRequired reads the flag file the Debian and Ubuntu maintainer
// scripts drop. The agent reports it; it never acts on it.
func (m *aptManager) RebootRequired(_ context.Context) (bool, error) {
	return fileExists(rebootRequiredFlag), nil
}
