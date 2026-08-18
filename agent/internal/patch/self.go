package patch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MinInstallTimeout is the shortest deadline an install job may carry. A
// package transaction that is interrupted is a host somebody has to repair by
// hand, so a job spec that cannot possibly hold one is refused before any
// package manager is started rather than after.
//
// The server's job layer should clamp to this too; enforcing it here as well
// means a bad spec cannot reach dpkg through any path.
const MinInstallTimeout = 30 * time.Minute

// ErrTimeoutTooShort means the job's deadline is below MinInstallTimeout.
var ErrTimeoutTooShort = errors.New("patch: install deadline is shorter than the floor")

// ErrSelfPackage means an id names the agent's own package. Installing it
// from inside a patch job runs the agent's own postinst, which restarts the
// service, which tears down the cgroup containing the package manager doing
// the installing. The agent updates itself through the signed self-update
// path instead, where the swap, the probation and the rollback are handled.
var ErrSelfPackage = errors.New("patch: the agent does not install its own package from a patch job")

// selfPackageNames are the names the agent ships under. Lower case; ids are
// folded before the lookup.
var selfPackageNames = map[string]bool{
	"everwas-agent": true,
	"everwas":       true,
}

// packageNameOf pulls the package name out of an update id in any of the
// backend formats: "pkg=version" (apt, pacman), "name.arch=evr" (dnf), or a
// bare label (softwareupdate, WUA GUIDs).
func packageNameOf(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	left, _, ok := strings.Cut(id, "=")
	if !ok {
		left = id
	}
	// dnf ids carry the architecture on the name half.
	if name, _ := splitNameArch(left); name != "" {
		left = name
	}
	return strings.TrimSpace(left)
}

// isSelfPackage reports whether an update id names the agent itself.
func isSelfPackage(id string) bool {
	name := packageNameOf(id)
	return name != "" && selfPackageNames[strings.ToLower(name)]
}

// refuseSelfPackages drops ids that name the agent's own package, recording
// the refusal against each one so the operator sees why rather than seeing a
// silently shorter list.
func refuseSelfPackages(ids []string, res *InstallResult) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if isSelfPackage(id) {
			res.fail(id, fmt.Errorf("%w: %s", ErrSelfPackage, id))
			continue
		}
		out = append(out, id)
	}
	return out
}

// checkInstallDeadline refuses an install whose context expires so soon that
// the transaction would be interrupted. Failing here is loud and harmless;
// failing halfway through dpkg is neither.
func checkInstallDeadline(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	remaining := time.Until(deadline)
	if remaining < MinInstallTimeout {
		return fmt.Errorf("%w: %s left, an install needs at least %s",
			ErrTimeoutTooShort, remaining.Round(time.Second), MinInstallTimeout)
	}
	return nil
}

// prepareInstall runs the checks every backend owes before it touches a
// package manager: a workable deadline, and no attempt to patch the agent
// through the patch path. It returns the ids that survived.
func prepareInstall(ctx context.Context, ids []string, res *InstallResult) ([]string, error) {
	if err := checkInstallDeadline(ctx); err != nil {
		for _, id := range ids {
			res.fail(id, err)
		}
		return nil, err
	}
	return refuseSelfPackages(ids, res), nil
}
