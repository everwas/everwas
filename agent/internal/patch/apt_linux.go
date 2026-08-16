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
	ids, err := prepareInstall(ctx, ids, &res)
	if err != nil {
		return res, err
	}
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

	cmdRes, runErr := m.runInstall(ctx, args, len(specs), progress)

	// Ask dpkg what actually landed, on the failure path as well as the
	// success path.
	attributeAptResult(want, idByName, m.installedVersions(ctx, want), runErr, cmdRes.tail(3), &res)
	res.RebootRequired = fileExists(rebootRequiredFlag)
	if runErr != nil && len(res.Installed) == 0 {
		return res, runErr
	}
	return res, nil
}

// attributeAptResult decides, per requested update, what happened. It takes
// dpkg's answer rather than apt's exit code because apt exits non-zero when
// ANY package in the transaction fails: attributing that to every id reports
// updates as not installed while dpkg has them on disk. The audit event is
// the change control record, and it used to be wrong in exactly the case an
// auditor cares about.
func attributeAptResult(want, idByName, installed map[string]string, runErr error, tail string, res *InstallResult) {
	for name, version := range want {
		id := idByName[name]
		switch got, ok := installed[name]; {
		case ok && got == version:
			res.Installed = append(res.Installed, id)
		case ok:
			res.fail(id, fmt.Errorf("installed version is %s, wanted %s", got, version))
		case runErr != nil:
			res.fail(id, fmt.Errorf("%w (dpkg does not have %s)", runErr, name))
		default:
			res.fail(id, errors.New("package not installed after apt-get install: "+tail))
		}
	}
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
			// A dpkg transaction that is killed halfway needs a human with
			// `dpkg --configure -a`, so the deadline reports a timeout and
			// lets the transaction finish. The scope keeps it out of the
			// agent's cgroup, so restarting the agent does not kill it
			// either.
			Scope:     true,
			LetFinish: true,
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
	// Detached from the job's context on purpose: the answer matters most
	// when the install was cut short, and a dead parent context would leave
	// us reporting "not installed" for packages dpkg already has.
	vctx, cancel := verifyContext(ctx)
	defer cancel()
	res := runCmd(vctx, execOptions{Timeout: quickTimeout}, "dpkg-query", args...)
	// dpkg-query exits 1 when any named package is unknown, but still
	// prints the ones it does know, so the output is used regardless.
	return parseDpkgVersions(res.Stdout)
}

// RebootRequired reads the flag file the Debian and Ubuntu maintainer
// scripts drop. The agent reports it; it never acts on it.
func (m *aptManager) RebootRequired(_ context.Context) (bool, error) {
	return fileExists(rebootRequiredFlag), nil
}
