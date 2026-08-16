package patch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// pacmanManager is best-effort support for Arch and its derivatives. A
// rolling release has no concept of a security-only update and no supported
// way to install one package at a pinned version, so this backend reports
// what is pending and installs the current version of whatever is approved.
// Partial upgrades are the one thing Arch genuinely does not support, so
// approving a subset here is the operator's call, not a safe default.
type pacmanManager struct {
	loggerHolder
	gate installGate
}

func (m *pacmanManager) Kind() string { return BackendPacman }

// Scan reads the pending list from the local sync database. It does NOT run
// `pacman -Sy`: refreshing the database without upgrading is how Arch hosts
// end up in a partial-upgrade state, so the scan is only as fresh as the
// last real sync.
func (m *pacmanManager) Scan(ctx context.Context) ([]Update, error) {
	res := runCmd(ctx, execOptions{Timeout: scanTimeout}, "pacman", "-Qu")
	if res.Err != nil {
		return nil, fmt.Errorf("pacman -Qu: %w", res.Err)
	}
	// pacman exits 1 with no output when nothing is pending, which is a
	// normal outcome and not an error.
	if res.ExitCode != 0 && strings.TrimSpace(res.Stdout) == "" {
		return []Update{}, nil
	}
	return parsePacmanQu(res.Stdout), nil
}

// Install upgrades the named packages. The version in the id is advisory:
// pacman installs whatever the sync database currently holds, so an id
// whose version has moved on still installs, and verification reports the
// version that actually landed.
func (m *pacmanManager) Install(ctx context.Context, ids []string, progress func(InstallProgress)) (InstallResult, error) {
	ids = dedupeIDs(ids)
	res := newInstallResult()
	if len(ids) == 0 {
		return res, nil
	}
	if !m.gate.acquire() {
		return res, ErrBusy
	}
	defer m.gate.release()

	// Scan first and install only what is actually pending. An approval is
	// permission to take a specific pending update, not a standing licence to
	// name any package, and a scan that fails is a reason to do nothing.
	available, err := m.Scan(ctx)
	if err != nil {
		return res, fmt.Errorf("pacman scan before install: %w", err)
	}

	before := m.installedVersions(ctx, nil)
	names, idByName := pacmanInstallPlan(ids, pacmanOfferedNames(available), &res)
	if len(names) == 0 {
		return res, nil
	}

	seen := 0
	// "--" ends option parsing: after it pacman cannot read a package name as
	// a flag, whatever gets past the validator in future.
	args := append([]string{"-S", "--noconfirm", "--needed", "--"}, names...)
	cmdRes := runCmd(ctx, execOptions{
		Timeout: installTimeout,
		OnLine: func(line string) {
			phase := PhaseInstall
			if strings.Contains(line, "downloading") {
				phase = PhaseDownload
			} else {
				seen++
			}
			emitProgress(progress, InstallProgress{Phase: phase, Pct: pctOf(seen, len(names))})
		},
	}, "pacman", args...)

	if cmdRes.Err != nil {
		err := fmt.Errorf("pacman -S: %w", cmdRes.Err)
		for _, id := range idByName {
			res.fail(id, err)
		}
		return res, err
	}

	after := m.installedVersions(ctx, names)
	for name, id := range idByName {
		got, ok := after[name]
		switch {
		case ok && got != before[name]:
			res.Installed = append(res.Installed, id)
		case ok && cmdRes.ExitCode == 0:
			// Already current: --needed makes this a no-op success.
			res.Installed = append(res.Installed, id)
		default:
			res.fail(id, fmt.Errorf("pacman -S exited %d: %s", cmdRes.ExitCode, cmdRes.tail(6)))
		}
	}
	rebootNeeded, _ := m.RebootRequired(ctx)
	res.RebootRequired = rebootNeeded
	return res, nil
}

// installedVersions queries pacman for the named packages, or all of them
// when names is nil.
func (m *pacmanManager) installedVersions(ctx context.Context, names []string) map[string]string {
	args := append([]string{"-Q"}, names...)
	res := runCmd(ctx, execOptions{Timeout: quickTimeout}, "pacman", args...)
	return parsePacmanQuery(res.Stdout)
}

// RebootRequired uses the standard Arch test: the running kernel's module
// directory disappears when the kernel package is upgraded underneath it,
// so its absence means the host is running a kernel it no longer has on
// disk.
func (m *pacmanManager) RebootRequired(_ context.Context) (bool, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return false, fmt.Errorf("uname: %w", err)
	}
	release := strings.TrimRight(string(uts.Release[:]), "\x00")
	if release == "" {
		return false, errors.New("empty kernel release")
	}
	if _, err := os.Stat("/usr/lib/modules/" + release); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}
