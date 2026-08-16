package patch

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// dnfManager drives dnf on Fedora, RHEL and their rebuilds.
type dnfManager struct {
	loggerHolder
	gate installGate
	// bin is "dnf" or "dnf5"; resolved at detection.
	bin string
}

func (m *dnfManager) Kind() string { return BackendDNF }

func (m *dnfManager) command() string {
	if m.bin == "" {
		return "dnf"
	}
	return m.bin
}

// Scan lists pending updates and enriches them with advisory severity when
// updateinfo is available (it needs the updateinfo metadata that RHEL and
// Fedora ship but some minimal mirrors do not).
func (m *dnfManager) Scan(ctx context.Context) ([]Update, error) {
	res := runCmd(ctx, execOptions{Timeout: scanTimeout}, m.command(), "-q", "check-update")
	if res.Err != nil {
		return nil, fmt.Errorf("dnf check-update: %w", res.Err)
	}
	switch res.ExitCode {
	case dnfExitNoUpdates:
		return []Update{}, nil
	case dnfExitUpdates:
		// fall through: this is the case with actual work in it
	case dnfExitError:
		return nil, fmt.Errorf("dnf check-update failed: %s", res.tail(5))
	default:
		return nil, fmt.Errorf("dnf check-update exited %d: %s", res.ExitCode, res.tail(5))
	}

	entries := parseDNFCheckUpdate(res.Stdout)
	return dnfUpdates(entries, m.advisories(ctx)), nil
}

// advisories reads severity metadata. It is optional: a repo without
// updateinfo yields an empty map and the scan degrades to unknown
// severities rather than failing.
func (m *dnfManager) advisories(ctx context.Context) map[string]dnfAdvisory {
	res := runCmd(ctx, execOptions{Timeout: scanTimeout},
		m.command(), "-q", "updateinfo", "list", "--with-cve")
	if res.Err != nil || (res.ExitCode != 0 && res.ExitCode != dnfExitUpdates) {
		m.logger().Debug("dnf updateinfo unavailable, severities will be unknown",
			"exit_code", res.ExitCode, "err", res.Err)
		return map[string]dnfAdvisory{}
	}
	return parseDNFUpdateinfo(res.Stdout)
}

// Install runs one dnf transaction for all requested updates, then checks
// per package what landed. One transaction rather than one per update:
// dnf resolves dependencies across the whole set, and installing them
// piecemeal invents conflicts that do not exist.
func (m *dnfManager) Install(ctx context.Context, ids []string, progress func(InstallProgress)) (InstallResult, error) {
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

	// Scan first and install only what dnf is actually offering, the way WUA
	// and softwareupdate already do.
	available, err := m.Scan(ctx)
	if err != nil {
		return res, fmt.Errorf("dnf scan before install: %w", err)
	}

	specs, idBySpec := dnfInstallPlan(ids, offeredIDs(available), &res)
	if len(specs) == 0 {
		return res, nil
	}

	seen := 0
	// "--" ends option parsing, so nothing in the package list can be read
	// as a dnf flag.
	args := append([]string{"-y", "install", "--"}, specs...)
	cmdRes := runCmd(ctx, execOptions{
		Timeout: installTimeout,
		// An rpm transaction killed halfway needs a human with `rpm --rebuilddb`
		// or worse, so the deadline reports a timeout and lets it finish, and
		// the scope keeps it out of the agent's cgroup.
		Scope:     true,
		LetFinish: true,
		OnLine: func(line string) {
			phase, ok := dnfProgressPhase(line)
			if !ok {
				return
			}
			if phase == PhaseInstall {
				seen++
			}
			emitProgress(progress, InstallProgress{Phase: phase, Pct: pctOf(seen, len(specs))})
		},
	}, m.command(), args...)

	rebootNeeded, _ := m.RebootRequired(ctx)
	res.RebootRequired = rebootNeeded

	if cmdRes.Err != nil {
		err := fmt.Errorf("dnf install: %w", cmdRes.Err)
		for _, id := range idBySpec {
			res.fail(id, err)
		}
		return res, err
	}

	installed := m.installedSpecs(ctx, specs)
	for spec, id := range idBySpec {
		if installed[spec] {
			res.Installed = append(res.Installed, id)
			continue
		}
		if cmdRes.ExitCode == 0 {
			res.fail(id, errors.New("dnf reported success but the package is not installed"))
			continue
		}
		res.fail(id, fmt.Errorf("dnf install exited %d: %s", cmdRes.ExitCode, cmdRes.tail(6)))
	}
	if cmdRes.ExitCode != 0 && len(res.Installed) == 0 {
		return res, fmt.Errorf("dnf install exited %d: %s", cmdRes.ExitCode, cmdRes.tail(8))
	}
	return res, nil
}

// installedSpecs asks rpm which of the requested nvras are now on disk.
func (m *dnfManager) installedSpecs(ctx context.Context, specs []string) map[string]bool {
	present := map[string]bool{}
	if !have("rpm") {
		// Without rpm we cannot verify, so trust the transaction. Better to
		// over-report success than to fail every id on a host missing a
		// tool that dnf itself depends on.
		for _, spec := range specs {
			present[spec] = true
		}
		return present
	}
	args := append([]string{"-q", "--qf", "%{NAME}-%{EVR}.%{ARCH}\n"}, specs...)
	// Detached from the job's context: what rpm has on disk is the truth we
	// need most when the install was cut short.
	vctx, cancel := verifyContext(ctx)
	defer cancel()
	res := runCmd(vctx, execOptions{Timeout: quickTimeout}, "rpm", args...)
	found := map[string]bool{}
	for line := range strings.Lines(res.Stdout) {
		found[strings.TrimSpace(line)] = true
	}
	for _, spec := range specs {
		// rpm normalises a zero epoch away, so compare on both forms.
		present[spec] = found[spec] || found[stripZeroEpoch(spec)]
	}
	return present
}

// RebootRequired uses `dnf needs-restarting -r`. Second exit-code trap of
// this file: needs-restarting returns 1 to mean REBOOT NEEDED, not
// failure, and 0 to mean the host is fine. A missing plugin also exits
// non-zero, so that case is detected from the message and reported as
// "unknown" rather than as a spurious reboot flag.
func (m *dnfManager) RebootRequired(ctx context.Context) (bool, error) {
	res := runCmd(ctx, execOptions{Timeout: quickTimeout}, m.command(), "needs-restarting", "-r")
	if res.Err == nil && !dnfPluginMissing(res.combined()) {
		return res.ExitCode == 1, nil
	}
	if have("needs-restarting") {
		res = runCmd(ctx, execOptions{Timeout: quickTimeout}, "needs-restarting", "-r")
		if res.Err == nil {
			return res.ExitCode == 1, nil
		}
	}
	return false, fmt.Errorf("needs-restarting unavailable: %s", res.tail(2))
}

// dnfProgressPhase classifies one line of live dnf output.
func dnfProgressPhase(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "Downloading Packages"),
		strings.HasPrefix(trimmed, "Downloading"):
		return PhaseDownload, true
	case strings.HasPrefix(trimmed, "Installing "),
		strings.HasPrefix(trimmed, "Upgrading "),
		strings.HasPrefix(trimmed, "Verifying  "),
		strings.HasPrefix(trimmed, "Running scriptlet"):
		return PhaseInstall, true
	default:
		return "", false
	}
}
