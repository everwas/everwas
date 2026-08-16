package patch

import (
	"fmt"
	"strings"
)

// dnf check-update exit codes. This is the classic trap: 100 is the
// SUCCESS case that means "updates are available", 0 means "nothing to do",
// and only 1 is a real error. Treating non-zero as failure here reports a
// broken scan on every host that actually has patches pending.
const (
	dnfExitNoUpdates = 0
	dnfExitError     = 1
	dnfExitUpdates   = 100
)

// dnfEntry is one pending update from `dnf check-update`.
type dnfEntry struct {
	Name string
	Arch string
	EVR  string // epoch:version-release, as dnf prints it
	Repo string
}

// nvra is the form dnf's own install command wants back.
func (e dnfEntry) nvra() string {
	if e.Arch == "" {
		return e.Name + "-" + e.EVR
	}
	return e.Name + "-" + e.EVR + "." + e.Arch
}

// id is the backend-native update id: name.arch=evr. It keeps the same
// "something=version" shape as apt so an operator reading the console does
// not have to learn two formats.
func (e dnfEntry) id() string {
	if e.Arch == "" {
		return e.Name + "=" + e.EVR
	}
	return e.Name + "." + e.Arch + "=" + e.EVR
}

// dnfInstallSpec converts an update id back into the nvra dnf expects.
func dnfInstallSpec(id string) (string, error) {
	left, evr, ok := strings.Cut(id, "=")
	if !ok || left == "" || evr == "" {
		return "", fmt.Errorf("malformed dnf update id %q, want name.arch=evr", id)
	}
	name, arch := splitNameArch(left)
	return dnfEntry{Name: name, Arch: arch, EVR: evr}.nvra(), nil
}

// dnfArches is the set of architecture suffixes dnf appends to a package
// name. It is an allowlist because package names contain dots too
// (python3.11-libs), so "last dot wins" alone would mangle them.
var dnfArches = map[string]bool{
	"x86_64": true, "i686": true, "i386": true, "noarch": true,
	"aarch64": true, "armv7hl": true, "armv6hl": true,
	"ppc64le": true, "ppc64": true, "s390x": true, "riscv64": true,
	"src": true,
}

// splitNameArch splits "bash.x86_64" into ("bash", "x86_64"). A token with
// no recognised arch suffix comes back whole, with an empty arch.
func splitNameArch(token string) (name, arch string) {
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		return token, ""
	}
	candidate := token[dot+1:]
	if !dnfArches[candidate] {
		return token, ""
	}
	return token[:dot], candidate
}

// dnfSkipPrefixes are the informational lines dnf prints even under -q on
// some releases.
var dnfSkipPrefixes = []string{
	"Last metadata expiration check",
	"Loaded plugins",
	"Dependencies resolved",
	"Security:",
	"Obsoleting Packages",
	"Updating Subscription Management",
	"Waiting for process with pid",
}

// parseDNFCheckUpdate parses `dnf -q check-update` output. Columns are
// name.arch, evr, repo. dnf wraps a long name onto its own line and puts
// the remaining two columns on the next one, which is handled here.
//
// Everything from the "Obsoleting Packages" section down is dropped: those
// are packages being replaced, not updates to install.
func parseDNFCheckUpdate(out string) []dnfEntry {
	entries := []dnfEntry{}
	var pending string // a wrapped name waiting for its evr and repo
	for raw := range strings.Lines(out) {
		line := strings.TrimRight(raw, "\r\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			pending = ""
			continue
		}
		if strings.HasPrefix(trimmed, "Obsoleting Packages") {
			break
		}
		if dnfSkipLine(trimmed) {
			pending = ""
			continue
		}
		fields := strings.Fields(trimmed)
		switch {
		case pending != "" && len(fields) == 2:
			name, arch := splitNameArch(pending)
			entries = append(entries, dnfEntry{Name: name, Arch: arch, EVR: fields[0], Repo: fields[1]})
			pending = ""
		case len(fields) == 1:
			// A wrapped package name. Only remember it if it looks like one.
			if strings.Contains(fields[0], ".") || strings.Contains(fields[0], "-") {
				pending = fields[0]
			}
		case len(fields) >= 3:
			name, arch := splitNameArch(fields[0])
			entries = append(entries, dnfEntry{Name: name, Arch: arch, EVR: fields[1], Repo: fields[2]})
			pending = ""
		default:
			pending = ""
		}
	}
	return entries
}

func dnfSkipLine(line string) bool {
	for _, p := range dnfSkipPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// dnfAdvisory is what updateinfo says about one package.
type dnfAdvisory struct {
	Severity string
	Security bool
}

// dnfSeverityTokens maps the words dnf uses onto our severity values.
var dnfSeverityTokens = map[string]string{
	"critical":  SeverityCritical,
	"important": SeverityImportant,
	"moderate":  SeverityModerate,
	"low":       SeverityLow,
}

// parseDNFUpdateinfo parses `dnf -q updateinfo list --with-cve` output,
// keyed by package nvra. The column layout has changed across dnf
// releases, so instead of assuming positions this looks for whichever
// field carries a severity token and takes the last field as the package:
//
//	RHSA-2023:1234 Important/Sec.  kernel-4.18.0-425.el8.x86_64
//	CVE-2023-1234  Important/Sec.  kernel-4.18.0-425.el8.x86_64
//	FEDORA-2023-ab CVE-2023-1234   Moderate/Sec. vim-9.0-1.fc38.x86_64
//
// A line with no severity token is not an advisory line (headers, summary
// counts, "Updateinfo list done") and is skipped.
func parseDNFUpdateinfo(out string) map[string]dnfAdvisory {
	advisories := map[string]dnfAdvisory{}
	for raw := range strings.Lines(out) {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 {
			continue
		}
		pkg := fields[len(fields)-1]
		severity, security, ok := dnfSeverityFromFields(fields[:len(fields)-1])
		if !ok {
			continue
		}
		prior, seen := advisories[pkg]
		adv := dnfAdvisory{Severity: severity, Security: security}
		if seen {
			// Two advisories can name the same package. Keep the worst
			// severity, and stay a security advisory if either one was.
			if severityRank(prior.Severity) >= severityRank(severity) {
				adv.Severity = prior.Severity
			}
			adv.Security = prior.Security || security
		}
		advisories[pkg] = adv
	}
	return advisories
}

// dnfSeverityFromFields looks for a "Important/Sec." style token among the
// leading fields.
func dnfSeverityFromFields(fields []string) (severity string, security bool, ok bool) {
	severity = SeverityUnknown
	for _, f := range fields {
		for _, part := range strings.Split(f, "/") {
			word := strings.ToLower(strings.Trim(part, ".,:"))
			if sev, isSev := dnfSeverityTokens[word]; isSev {
				severity, ok = sev, true
				continue
			}
			if word == "sec" || word == "security" {
				security, ok = true, true
			}
		}
	}
	return severity, security, ok
}

// severityRank orders severities so the worst one wins a merge.
func severityRank(s string) int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityImportant:
		return 3
	case SeverityModerate:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// dnfRebootPrefixes name packages whose upgrade needs a reboot to take
// effect on an RPM host.
var dnfRebootPrefixes = []string{
	"kernel", "glibc", "systemd", "dbus", "linux-firmware",
	"microcode_ctl", "grub2", "shim", "openssl-libs",
}

func dnfRebootLikely(name string) bool {
	for _, p := range dnfRebootPrefixes {
		if name == p || strings.HasPrefix(name, p+"-") {
			return true
		}
	}
	return false
}

// stripZeroEpoch removes a "0:" epoch from an nvra, which is how rpm -q
// prints a package that dnf listed with an explicit zero epoch.
func stripZeroEpoch(spec string) string {
	name, rest, ok := strings.Cut(spec, "-0:")
	if !ok {
		return spec
	}
	return name + "-" + rest
}

// dnfPluginMissing recognises dnf's "no such command" reply, which is what
// a host without dnf-plugins-core says. Without this the missing plugin's
// non-zero exit would be read as "reboot needed" on every check.
func dnfPluginMissing(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "no such command") ||
		strings.Contains(low, "unknown command") ||
		strings.Contains(low, "command not found")
}

// dnfUpdates joins check-update entries with updateinfo advisories.
func dnfUpdates(entries []dnfEntry, advisories map[string]dnfAdvisory) []Update {
	updates := make([]Update, 0, len(entries))
	for _, e := range entries {
		u := Update{
			ID:           e.id(),
			Title:        e.nvra(),
			Kind:         KindOther,
			Severity:     SeverityUnknown,
			RebootLikely: dnfRebootLikely(e.Name),
		}
		if adv, ok := advisories[e.nvra()]; ok {
			u.Severity = adv.Severity
			if adv.Security {
				u.Kind = KindSecurity
			}
		}
		updates = append(updates, u)
	}
	return updates
}
