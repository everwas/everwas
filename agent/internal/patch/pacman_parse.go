package patch

import "strings"

// pacmanRebootNames are the packages whose upgrade leaves an Arch host
// needing a reboot.
var pacmanRebootNames = map[string]bool{
	"linux": true, "linux-lts": true, "linux-zen": true, "linux-hardened": true,
	"linux-firmware": true, "systemd": true, "glibc": true, "dbus": true,
	"nvidia": true, "nvidia-dkms": true,
}

// parsePacmanQu parses `pacman -Qu` output:
//
//	linux 6.9.1.arch1-1 -> 6.9.2.arch1-1
//	foo 1.0-1 -> 2.0-1 [ignored]
//
// Packages marked [ignored] are held by the operator's IgnorePkg config and
// are skipped: reporting an update the host is configured never to take
// would show as permanently pending in the console.
func parsePacmanQu(out string) []Update {
	updates := []Update{}
	for raw := range strings.Lines(out) {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "->" {
			continue
		}
		if len(fields) > 4 && strings.Contains(strings.ToLower(fields[4]), "ignored") {
			continue
		}
		name, oldVer, newVer := fields[0], fields[1], fields[3]
		if name == "" || newVer == "" {
			continue
		}
		updates = append(updates, Update{
			ID:           name + "=" + newVer,
			Title:        name + " " + oldVer + " to " + newVer,
			Kind:         KindOther, // Arch publishes no per-package advisory feed
			Severity:     SeverityUnknown,
			RebootLikely: pacmanRebootNames[name],
		})
	}
	return updates
}

// parsePacmanQuery parses `pacman -Q name...` output ("name version" per
// line), used to verify what an install actually left on disk.
func parsePacmanQuery(out string) map[string]string {
	versions := map[string]string{}
	for raw := range strings.Lines(out) {
		name, version, ok := strings.Cut(strings.TrimSpace(raw), " ")
		if !ok || name == "" {
			continue
		}
		versions[name] = strings.TrimSpace(version)
	}
	return versions
}
