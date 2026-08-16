package patch

import (
	"strings"
)

// aptInst is one parsed "Inst " line from `apt-get -s dist-upgrade`.
type aptInst struct {
	Name       string // may carry a :arch multiarch qualifier
	OldVersion string // empty for a package that is not installed yet
	NewVersion string
	Origins    string // e.g. "Debian-Security:11/oldstable" or a comma list
	Arch       string
}

// parseAptSimulate turns the output of
//
//	apt-get -s -o Debug::NoLocking=1 dist-upgrade
//
// into updates. Only "Inst " lines matter; "Conf", "Remv" and the human
// summary are ignored. A line we cannot make sense of is skipped rather
// than failing the scan: apt's simulate output is not a stable interface
// and one odd package must not blind us to the other ninety.
func parseAptSimulate(out string) []Update {
	updates := []Update{}
	for line := range strings.Lines(out) {
		inst, ok := parseAptInstLine(line)
		if !ok {
			continue
		}
		updates = append(updates, inst.update())
	}
	return updates
}

// update converts one Inst line into the wire shape.
func (i aptInst) update() Update {
	kind := KindOther
	if aptIsSecurityOrigin(i.Origins) {
		kind = KindSecurity
	}
	title := i.Name + " " + i.NewVersion
	if i.OldVersion != "" {
		title = i.Name + " " + i.OldVersion + " to " + i.NewVersion
	}
	return Update{
		ID:           i.Name + "=" + i.NewVersion,
		Title:        title,
		Kind:         kind,
		Severity:     SeverityUnknown, // apt does not publish a severity
		RebootLikely: aptRebootLikely(i.Name),
	}
}

// parseAptInstLine parses one line of the form
//
//	Inst libc6 [2.31-13] (2.31-13+deb11u5 Debian-Security:11/oldstable [amd64])
//	Inst newpkg (1.0-1 Ubuntu:22.04/jammy [amd64]) []
//	Inst libc6:i386 [2.31-13] (2.31-13+deb11u5 Debian:11/stable, Debian-Security:11/oldstable [amd64])
func parseAptInstLine(line string) (aptInst, bool) {
	line = strings.TrimSpace(line)
	rest, found := strings.CutPrefix(line, "Inst ")
	if !found {
		return aptInst{}, false
	}
	name, rest, found := strings.Cut(strings.TrimSpace(rest), " ")
	if !found || name == "" {
		return aptInst{}, false
	}
	inst := aptInst{Name: name}
	rest = strings.TrimSpace(rest)

	// Optional [old-version] before the parenthesised candidate.
	if strings.HasPrefix(rest, "[") {
		old, after, ok := strings.Cut(rest[1:], "]")
		if !ok {
			return aptInst{}, false
		}
		inst.OldVersion = strings.TrimSpace(old)
		rest = strings.TrimSpace(after)
	}

	open := strings.Index(rest, "(")
	if open < 0 {
		return aptInst{}, false
	}
	// LastIndex, not Index: the trailing " []" is not a paren, but a version
	// or origin containing ")" would break a naive first-match.
	closeIdx := strings.LastIndex(rest, ")")
	if closeIdx <= open {
		return aptInst{}, false
	}
	body := strings.TrimSpace(rest[open+1 : closeIdx])
	version, after, ok := strings.Cut(body, " ")
	if !ok || version == "" {
		return aptInst{}, false
	}
	inst.NewVersion = version

	after = strings.TrimSpace(after)
	if bracket := strings.LastIndex(after, "["); bracket >= 0 {
		arch := strings.TrimSuffix(strings.TrimSpace(after[bracket+1:]), "]")
		inst.Arch = strings.TrimSpace(arch)
		after = strings.TrimSpace(after[:bracket])
	}
	inst.Origins = strings.TrimSpace(after)
	return inst, true
}

// aptIsSecurityOrigin decides whether an origin blob names a security
// archive. One rule covers both families: Debian publishes
// "Debian-Security:11/oldstable" and Ubuntu publishes
// "Ubuntu:22.04/jammy-security", and both contain "-security" once the
// string is lowercased.
func aptIsSecurityOrigin(origins string) bool {
	return strings.Contains(strings.ToLower(origins), "-security")
}

// aptRebootPrefixes are package name prefixes that always mean a reboot.
var aptRebootPrefixes = []string{
	"linux-image", "linux-generic", "linux-modules", "linux-headers",
	"linux-aws", "linux-azure", "linux-gcp", "linux-oracle", "linux-kvm",
	"linux-firmware", "linux-lowlatency",
}

// aptRebootNames are exact package names whose upgrade leaves the running
// system inconsistent until a reboot.
var aptRebootNames = map[string]bool{
	"libc6":             true,
	"libc-bin":          true,
	"systemd":           true,
	"udev":              true,
	"dbus":              true,
	"grub-common":       true,
	"grub-pc":           true,
	"grub-efi-amd64":    true,
	"xserver-xorg-core": true,
}

// aptRebootLikely is a heuristic, not a promise: it is what the console
// shows an operator so they can plan a window.
func aptRebootLikely(name string) bool {
	base, _, _ := strings.Cut(name, ":") // drop a multiarch qualifier
	if aptRebootNames[base] {
		return true
	}
	for _, p := range aptRebootPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// splitPkgVersionID splits an "pkg=version" id. It is shared by apt and
// pacman, which both use that form.
func splitPkgVersionID(id string) (name, version string, ok bool) {
	name, version, ok = strings.Cut(id, "=")
	if !ok || name == "" || version == "" {
		return "", "", false
	}
	return name, version, true
}

// aptTransientMarkers are the `apt-get update` failures worth retrying:
// DNS, a mirror that is mid-sync, a proxy hiccup. Anything else (a broken
// sources.list, a missing GPG key) will fail identically three times.
var aptTransientMarkers = []string{
	"temporary failure resolving",
	"could not connect",
	"connection timed out",
	"connection failed",
	"failed to fetch",
	"hash sum mismatch",
	"file has unexpected size",
	"unable to connect",
	"503 service unavailable",
	"502 bad gateway",
	"could not resolve",
}

func aptIsTransientUpdateError(text string) bool {
	low := strings.ToLower(text)
	for _, m := range aptTransientMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// aptLockMarkers identify dpkg/apt lock contention. unattended-upgrades,
// a package kit daemon or a human at a terminal all produce one of these.
var aptLockMarkers = []string{
	"could not get lock",
	"unable to acquire the dpkg frontend lock",
	"dpkg frontend lock",
	"resource temporarily unavailable",
	"unable to lock the administration directory",
	"another process is using it",
	"is another process using it",
}

func aptIsLockContention(text string) bool {
	low := strings.ToLower(text)
	for _, m := range aptLockMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// parseDpkgVersions parses "name<TAB>version" lines from dpkg-query, which
// is how an install verifies what actually landed. Lines dpkg emits for
// unknown packages ("no packages found matching ...") go to stderr and
// never reach here.
func parseDpkgVersions(out string) map[string]string {
	versions := map[string]string{}
	for line := range strings.Lines(out) {
		name, version, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		if !ok || name == "" || version == "" {
			continue
		}
		versions[name] = version
	}
	return versions
}

// aptProgressPhase classifies one line of live apt output. Get: lines are
// the download stage; unpack and configure are the install stage.
func aptProgressPhase(line string) (phase string, ok bool) {
	switch {
	case strings.HasPrefix(line, "Get:"):
		return PhaseDownload, true
	case strings.HasPrefix(line, "Unpacking "),
		strings.HasPrefix(line, "Preparing to unpack "),
		strings.HasPrefix(line, "Setting up "):
		return PhaseInstall, true
	default:
		return "", false
	}
}
