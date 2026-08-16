package patch

import (
	"errors"
	"fmt"
	"strings"
)

// maxPkgTokenLen bounds a name or a version. Real ones are far shorter; the
// limit exists so a pathological id cannot build a command line that fails
// in some other, less obvious way.
const maxPkgTokenLen = 256

// ErrBadUpdateID is wrapped by every id rejection, so a caller can tell a
// malformed request apart from a package manager that failed.
var ErrBadUpdateID = errors.New("patch: unusable update id")

// ErrNotOffered means the backend is not currently offering the update that
// was asked for. It is the same refusal WUA and softwareupdate already make:
// an approval is permission to install a specific pending update, not a
// standing licence to name any package.
var ErrNotOffered = errors.New("patch: update is not currently offered by this host")

// checkPkgName validates a package name that came off the wire before it is
// placed in an argv.
//
// The first character must be alphanumeric, which is what actually matters:
// every package manager here uses getopt-style parsing, so a name beginning
// with a dash is not a package at all, it is an option. An id of "-Syu" fell
// through pacman's install path as a bare package name and became
// `pacman -S --noconfirm --needed -Syu`, which pacman reads as a full
// unattended system upgrade with no targets, as root, on a host where
// nothing was approved.
//
// allowUpper is for rpm, where uppercase names are ordinary
// (NetworkManager, ModemManager, GConf2). Debian and Arch both require
// lowercase, so they get the tighter grammar.
func checkPkgName(name string, allowUpper bool) error {
	if name == "" {
		return fmt.Errorf("%w: empty package name", ErrBadUpdateID)
	}
	if len(name) > maxPkgTokenLen {
		return fmt.Errorf("%w: package name is %d bytes, limit %d",
			ErrBadUpdateID, len(name), maxPkgTokenLen)
	}
	for i, r := range name {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			(allowUpper && r >= 'A' && r <= 'Z')
		if i == 0 {
			if !alnum {
				return fmt.Errorf("%w: package name %q must start with a letter or digit, "+
					"otherwise the package manager reads it as an option", ErrBadUpdateID, name)
			}
			continue
		}
		if alnum || r == '@' || r == '.' || r == '_' || r == '+' || r == '-' {
			continue
		}
		return fmt.Errorf("%w: package name %q contains %q", ErrBadUpdateID, name, string(r))
	}
	return nil
}

// checkPkgVersion validates the version half of an id. Debian, rpm and Arch
// versions between them use alphanumerics plus ".:+~^_-", and nothing else
// is a version, whatever else it might be.
func checkPkgVersion(version string) error {
	if version == "" {
		return fmt.Errorf("%w: empty version", ErrBadUpdateID)
	}
	if len(version) > maxPkgTokenLen {
		return fmt.Errorf("%w: version is %d bytes, limit %d",
			ErrBadUpdateID, len(version), maxPkgTokenLen)
	}
	if strings.HasPrefix(version, "-") {
		return fmt.Errorf("%w: version %q starts with a dash", ErrBadUpdateID, version)
	}
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == ':', r == '+', r == '~', r == '^', r == '_', r == '-':
		default:
			return fmt.Errorf("%w: version %q contains %q", ErrBadUpdateID, version, string(r))
		}
	}
	return nil
}

// offeredIDs indexes a scan by update id, so an install can refuse anything
// the host is not currently offering.
func offeredIDs(updates []Update) map[string]bool {
	set := make(map[string]bool, len(updates))
	for _, u := range updates {
		set[u.ID] = true
	}
	return set
}

// aptInstallPlan turns requested ids into "pkg=version" specs, recording a
// per-id reason for everything it drops. offered is the id set from a fresh
// scan; an id that is not in it is refused rather than handed to apt.
func aptInstallPlan(ids []string, offered map[string]bool, res *InstallResult) (specs []string, want, idByName map[string]string) {
	specs = make([]string, 0, len(ids))
	want = make(map[string]string, len(ids))
	idByName = make(map[string]string, len(ids))
	for _, id := range ids {
		name, version, ok := splitPkgVersionID(id)
		if !ok {
			res.fail(id, fmt.Errorf("%w: %q, want pkg=version", ErrBadUpdateID, id))
			continue
		}
		if err := checkPkgName(name, false); err != nil {
			res.fail(id, err)
			continue
		}
		if err := checkPkgVersion(version); err != nil {
			res.fail(id, err)
			continue
		}
		if !offered[id] {
			res.fail(id, fmt.Errorf("%w: apt is not offering %s", ErrNotOffered, id))
			continue
		}
		specs = append(specs, name+"="+version)
		want[name] = version
		idByName[name] = id
	}
	return specs, want, idByName
}

// dnfInstallPlan turns requested ids into nvra specs, refusing anything dnf
// is not currently offering.
func dnfInstallPlan(ids []string, offered map[string]bool, res *InstallResult) (specs []string, idBySpec map[string]string) {
	specs = make([]string, 0, len(ids))
	idBySpec = make(map[string]string, len(ids))
	for _, id := range ids {
		left, evr, ok := strings.Cut(id, "=")
		if !ok || left == "" || evr == "" {
			res.fail(id, fmt.Errorf("%w: %q, want name.arch=evr", ErrBadUpdateID, id))
			continue
		}
		name, _ := splitNameArch(left)
		if err := checkPkgName(name, true); err != nil {
			res.fail(id, err)
			continue
		}
		if err := checkPkgVersion(evr); err != nil {
			res.fail(id, err)
			continue
		}
		if !offered[id] {
			res.fail(id, fmt.Errorf("%w: dnf is not offering %s", ErrNotOffered, id))
			continue
		}
		spec, err := dnfInstallSpec(id)
		if err != nil {
			res.fail(id, err)
			continue
		}
		specs = append(specs, spec)
		idBySpec[spec] = id
	}
	return specs, idBySpec
}

// pacmanInstallPlan turns requested ids into package names.
//
// A bare package name is NOT accepted: the id form is pkg=version, and the
// old "tolerate a bare name" fallback is exactly what let an option through.
// The version is still advisory (pacman installs whatever the sync database
// holds), so the offered check is by NAME. Requiring the version to match
// would refuse every id the moment a mirror moved, on a rolling release
// where that is the normal state of the world.
func pacmanInstallPlan(ids []string, offeredNames map[string]bool, res *InstallResult) (names []string, idByName map[string]string) {
	names = make([]string, 0, len(ids))
	idByName = make(map[string]string, len(ids))
	for _, id := range ids {
		name, version, ok := splitPkgVersionID(id)
		if !ok {
			res.fail(id, fmt.Errorf("%w: %q, want pkg=version", ErrBadUpdateID, id))
			continue
		}
		if err := checkPkgName(name, false); err != nil {
			res.fail(id, err)
			continue
		}
		if err := checkPkgVersion(version); err != nil {
			res.fail(id, err)
			continue
		}
		if !offeredNames[name] {
			res.fail(id, fmt.Errorf("%w: pacman is not offering an update for %s", ErrNotOffered, name))
			continue
		}
		if _, seen := idByName[name]; seen {
			continue
		}
		names = append(names, name)
		idByName[name] = id
	}
	return names, idByName
}

// pacmanOfferedNames is the package-name set from a pacman scan.
func pacmanOfferedNames(updates []Update) map[string]bool {
	set := make(map[string]bool, len(updates))
	for _, u := range updates {
		if name, _, ok := splitPkgVersionID(u.ID); ok {
			set[name] = true
		}
	}
	return set
}
