package patch

import (
	"strconv"
	"strings"
)

// macOSHost is what the parser needs to know about the machine it is
// describing. MajorVersion is 0 when it could not be determined, which
// makes the major-upgrade test fall back to "assume upgrade" for anything
// the host cannot vouch for.
type macOSHost struct {
	AppleSilicon bool
	MajorVersion int
}

// suBlock is one raw `* Label:` block before classification.
type suBlock struct {
	Label   string
	Attrs   map[string]string
	RawSize string
}

// detailAppleSilicon is the reason an update we can see cannot be driven
// from a headless agent on Apple Silicon: softwareupdate needs a volume
// owner's credentials to authorise a system update.
const detailAppleSilicon = "requires MDM or a local admin session"

// detailMajorUpgrade is why major upgrades are never attempted in v1.
const detailMajorUpgrade = "major macOS upgrades are not installed by the agent"

// parseSoftwareUpdateList parses `softwareupdate --list` output. The block
// format has drifted across macOS releases (sizes went from "89747K" to
// "6081868KiB", the Title line gained and lost fields, macOS 14 dropped the
// "found the following" banner), so this parses by key rather than by
// column and tolerates unknown keys.
//
// A block it cannot make sense of is returned in degraded rather than
// aborting the scan: one weird entry must not cost us the rest of the list.
func parseSoftwareUpdateList(out string, host macOSHost) (updates []Update, degraded []string) {
	updates = []Update{}
	blocks, bad := splitSoftwareUpdateBlocks(out)
	degraded = bad
	for _, b := range blocks {
		u, ok := b.update(host)
		if !ok {
			degraded = append(degraded, b.Label)
			continue
		}
		updates = append(updates, u)
	}
	return updates, degraded
}

// splitSoftwareUpdateBlocks turns the raw listing into label plus attribute
// blocks.
func splitSoftwareUpdateBlocks(out string) (blocks []suBlock, degraded []string) {
	var cur *suBlock
	flush := func() {
		if cur == nil {
			return
		}
		if len(cur.Attrs) == 0 {
			degraded = append(degraded, cur.Label)
		} else {
			blocks = append(blocks, *cur)
		}
		cur = nil
	}
	for raw := range strings.Lines(out) {
		line := strings.TrimRight(raw, "\r\n")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if label, ok := cutSoftwareUpdateLabel(trimmed); ok {
			flush()
			if label == "" {
				degraded = append(degraded, "(empty label)")
				continue
			}
			cur = &suBlock{Label: label, Attrs: map[string]string{}}
			continue
		}
		if cur == nil {
			continue // banner lines before the first block
		}
		// Attribute lines are indented under their label. A non-indented
		// line that is not a label ends the block (trailing summary text).
		if line == trimmed {
			flush()
			continue
		}
		for k, v := range parseSoftwareUpdateAttrs(trimmed) {
			cur.Attrs[k] = v
		}
	}
	flush()
	return blocks, degraded
}

// cutSoftwareUpdateLabel matches the "* Label: name" line. macOS 12 and 13
// use "* Label:"; some builds print a bare "*" bullet with the label on the
// same line, which is the same thing once the marker is cut.
func cutSoftwareUpdateLabel(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "* Label:")
	if !ok {
		if rest, ok = strings.CutPrefix(line, "*Label:"); !ok {
			return "", false
		}
	}
	return strings.TrimSpace(rest), true
}

// parseSoftwareUpdateAttrs parses "Title: Safari, Version: 17.5, Size:
// 128173KiB, Recommended: YES," into a map. Splitting on ", " alone would
// break a title containing a comma, so a fragment with no "Key: " prefix is
// appended to the previous value instead of being dropped.
func parseSoftwareUpdateAttrs(line string) map[string]string {
	attrs := map[string]string{}
	lastKey := ""
	for _, frag := range strings.Split(line, ", ") {
		frag = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(frag), ","))
		if frag == "" {
			continue
		}
		key, value, ok := strings.Cut(frag, ": ")
		if !ok || strings.ContainsAny(key, " \t") && lastKey != "" {
			if lastKey != "" {
				attrs[lastKey] += ", " + frag
			}
			continue
		}
		key = strings.TrimSpace(key)
		attrs[key] = strings.TrimSpace(value)
		lastKey = key
	}
	return attrs
}

// update classifies one block. It reports false when the block carries
// nothing usable.
func (b suBlock) update(host macOSHost) (Update, bool) {
	if b.Label == "" || len(b.Attrs) == 0 {
		return Update{}, false
	}
	title := b.Attrs["Title"]
	version := b.Attrs["Version"]
	if title == "" {
		title = b.Label
	}
	if version != "" && !strings.Contains(title, version) {
		title = title + " " + version
	}
	u := Update{
		ID:           b.Label,
		Title:        title,
		Kind:         KindOther,
		Severity:     SeverityUnknown, // Apple publishes no per-update severity
		SizeBytes:    parseSoftwareUpdateSize(b.Attrs["Size"]),
		RebootLikely: strings.EqualFold(b.Attrs["Action"], "restart"),
	}
	u.Kind, u.Unsupported, u.Detail = classifySoftwareUpdate(b.Label, title, version, host)
	return u, true
}

// macOSDefinitionPrefixes are the background data updates Apple ships
// through the same channel as real software updates.
var macOSDefinitionPrefixes = []string{
	"XProtect", "MRTConfigData", "Gatekeeper", "ConfigData", "XProtectPlistConfigData",
}

// classifySoftwareUpdate decides kind and whether the agent may install it.
//
// Two things are never installed by the agent in v1: a major macOS upgrade
// (a version jump, which changes the machine out from under everything
// running on it), and any OS update on Apple Silicon (softwareupdate needs
// a volume owner to authorise it, which a headless daemon is not).
func classifySoftwareUpdate(label, title, version string, host macOSHost) (kind string, unsupported bool, detail string) {
	kind = KindOther
	lowLabel := strings.ToLower(label)
	lowTitle := strings.ToLower(title)

	for _, p := range macOSDefinitionPrefixes {
		if strings.HasPrefix(label, p) {
			return KindDefinition, false, ""
		}
	}

	isOS := strings.HasPrefix(label, "macOS ") || strings.HasPrefix(title, "macOS ")
	switch {
	case strings.Contains(lowTitle, "security response"),
		strings.Contains(lowTitle, "security update"),
		strings.Contains(lowLabel, "securityupd"):
		kind = KindSecurity
	case strings.HasPrefix(lowLabel, "safari"), strings.HasPrefix(lowTitle, "safari"):
		kind = KindSecurity
	case isOS:
		kind = KindSecurity // Apple ships CVE fixes inside OS point updates
	}

	if !isOS {
		return kind, false, ""
	}
	if isMajorMacOSUpgrade(version, host.MajorVersion) {
		return KindFeature, true, detailMajorUpgrade
	}
	if host.AppleSilicon {
		return kind, true, detailAppleSilicon
	}
	return kind, false, ""
}

// isMajorMacOSUpgrade compares the update's major version against the
// running one. An unknown running version (0) or an unparseable update
// version is treated as an upgrade: refusing to install is the safe answer
// when we cannot tell.
func isMajorMacOSUpgrade(version string, runningMajor int) bool {
	if runningMajor <= 0 {
		return true
	}
	head, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil {
		return true
	}
	return major > runningMajor
}

// sizeUnits maps softwareupdate's size suffixes onto a byte multiplier.
// macOS 12 prints "89747K" (kibibytes despite the suffix); 13 and later
// print "6081868KiB". Both mean the same thing.
var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"KiB", 1024},
	{"MiB", 1024 * 1024},
	{"GiB", 1024 * 1024 * 1024},
	{"KB", 1024},
	{"MB", 1024 * 1024},
	{"GB", 1024 * 1024 * 1024},
	{"K", 1024},
	{"M", 1024 * 1024},
	{"G", 1024 * 1024 * 1024},
	{"B", 1},
}

// parseSoftwareUpdateSize converts a size field into bytes, returning 0 for
// anything it cannot read. A missing size is cosmetic; it never fails a scan.
func parseSoftwareUpdateSize(field string) int64 {
	field = strings.TrimSpace(field)
	if field == "" {
		return 0
	}
	for _, u := range sizeUnits {
		digits, ok := strings.CutSuffix(field, u.suffix)
		if !ok {
			continue
		}
		return parseSizeDigits(strings.TrimSpace(digits), u.mult)
	}
	return parseSizeDigits(field, 1)
}

func parseSizeDigits(digits string, mult int64) int64 {
	if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
		return n * mult
	}
	if f, err := strconv.ParseFloat(digits, 64); err == nil && f >= 0 {
		return int64(f * float64(mult))
	}
	return 0
}

// parseSWVersMajor reads the major version out of `sw_vers -productVersion`
// output ("15.5" -> 15). It returns 0 when the output makes no sense.
func parseSWVersMajor(out string) int {
	head, _, _ := strings.Cut(strings.TrimSpace(out), ".")
	major, err := strconv.Atoi(head)
	if err != nil || major <= 0 {
		return 0
	}
	return major
}

// softwareUpdateInstallFailed recognises the failure text softwareupdate
// prints when it cannot authorise an install. It exits 0 in some of these
// cases, so the exit code alone would report a phantom success.
var softwareUpdateFailureMarkers = []string{
	"failed to install",
	"could not be installed",
	"an error occurred",
	"not authorized",
	"authentication is required",
	"must be run as root",
	"no such update",
	"update not found",
}

func softwareUpdateInstallFailed(text string) bool {
	low := strings.ToLower(text)
	for _, m := range softwareUpdateFailureMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
