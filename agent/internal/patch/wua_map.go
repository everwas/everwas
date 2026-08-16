package patch

import (
	"fmt"
	"strings"
)

// InstallationRebootBehavior values from the Windows Update Agent API.
const (
	wuaNeverReboots         = 0
	wuaAlwaysRequiresReboot = 1
	wuaCanRequestReboot     = 2
)

// OperationResultCode values. Anything other than Succeeded and
// SucceededWithErrors means the update did not go on.
const (
	wuaResultNotStarted          = 0
	wuaResultInProgress          = 1
	wuaResultSucceeded           = 2
	wuaResultSucceededWithErrors = 3
	wuaResultFailed              = 4
	wuaResultAborted             = 5
)

// wuaSearchCriteria is the search every scan and install runs. Type
// 'Software' excludes drivers, which an RMM has no business pushing
// silently; IsHidden=0 respects updates an administrator has hidden on the
// box.
const wuaSearchCriteria = "IsInstalled=0 and IsHidden=0 and Type='Software'"

// wuaFields is one IUpdate flattened out of COM. Keeping this struct
// between the COM layer and the mapping logic is what makes the mapping
// testable on a machine that has no Windows Update Agent.
type wuaFields struct {
	UpdateID            string
	RevisionNumber      int64
	Title               string
	MsrcSeverity        string
	KBIDs               []string
	Categories          []string
	MaxDownloadSize     int64
	RebootBehavior      int64
	EulaAccepted        bool
	CanRequestUserInput bool
	Description         string
}

// detailUserInput is why an update that would put a dialog on the console
// is reported but never installed.
const detailUserInput = "requires user interaction, cannot be installed silently"

// wuaUpdateFromFields converts a search result into the wire shape.
func wuaUpdateFromFields(f wuaFields) Update {
	u := Update{
		ID:           f.UpdateID,
		Title:        strings.TrimSpace(f.Title),
		Kind:         wuaKind(f.Categories, f.Title),
		KBIDs:        normalizeKBIDs(f.KBIDs),
		Severity:     mapMsrcSeverity(f.MsrcSeverity),
		SizeBytes:    f.MaxDownloadSize,
		RebootLikely: wuaRebootLikely(f.RebootBehavior),
	}
	if f.CanRequestUserInput {
		u.Unsupported = true
		u.Detail = detailUserInput
	}
	return u
}

// mapMsrcSeverity maps Microsoft's MsrcSeverity strings onto our values.
// "Unspecified" and an empty string both mean Microsoft did not rate it,
// which is unknown rather than low.
func mapMsrcSeverity(msrc string) string {
	switch strings.ToLower(strings.TrimSpace(msrc)) {
	case "critical":
		return SeverityCritical
	case "important":
		return SeverityImportant
	case "moderate":
		return SeverityModerate
	case "low":
		return SeverityLow
	default:
		return SeverityUnknown
	}
}

// wuaRebootLikely treats "can request a reboot" as likely: the console is
// telling an operator whether to plan a window, and a maybe belongs in the
// window.
func wuaRebootLikely(behavior int64) bool {
	return behavior == wuaAlwaysRequiresReboot || behavior == wuaCanRequestReboot
}

// wuaKind classifies an update from its categories, falling back to the
// title when a host reports no categories at all.
//
// Definition updates are checked first: Defender's definitions carry both
// the "Definition Updates" category and a security-sounding title, and
// filing thousands of them as security updates would bury the real ones.
func wuaKind(categories []string, title string) string {
	var lower []string
	for _, c := range categories {
		lower = append(lower, strings.ToLower(c))
	}
	switch {
	case containsAny(lower, "definition updates", "definition update"):
		return KindDefinition
	case containsAny(lower, "security updates", "critical updates"):
		return KindSecurity
	case containsAny(lower, "feature packs", "upgrades", "service packs"):
		return KindFeature
	}
	low := strings.ToLower(title)
	switch {
	case strings.Contains(low, "security intelligence update"),
		strings.Contains(low, "definition update"),
		strings.Contains(low, "antimalware platform"):
		return KindDefinition
	case strings.Contains(low, "security update"), strings.Contains(low, "security monthly"):
		return KindSecurity
	case strings.Contains(low, "feature update"):
		return KindFeature
	default:
		return KindOther
	}
}

func containsAny(haystack []string, needles ...string) bool {
	for _, h := range haystack {
		for _, n := range needles {
			if h == n {
				return true
			}
		}
	}
	return false
}

// normalizeKBIDs puts the "KB" prefix back on. WUA returns bare article
// numbers ("5034441"), but every human, ticket and vendor advisory writes
// them as KB5034441.
func normalizeKBIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(id), "KB") {
			id = "KB" + id
		} else {
			id = "KB" + id[2:]
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// wuaResultOK reports whether an OperationResultCode means the update went
// on. SucceededWithErrors counts: the update installed and something
// cosmetic failed after it.
func wuaResultOK(code int64) bool {
	return code == wuaResultSucceeded || code == wuaResultSucceededWithErrors
}

// wuaResultText renders an OperationResultCode plus its HRESULT for an
// operator. The raw numbers are what Microsoft's own documentation is
// indexed by, so they stay in the message.
func wuaResultText(code int64, hresult int64) string {
	var name string
	switch code {
	case wuaResultNotStarted:
		name = "not started"
	case wuaResultInProgress:
		name = "still in progress"
	case wuaResultSucceeded:
		name = "succeeded"
	case wuaResultSucceededWithErrors:
		name = "succeeded with errors"
	case wuaResultFailed:
		name = "failed"
	case wuaResultAborted:
		name = "aborted"
	default:
		name = "unknown result"
	}
	if hresult == 0 {
		return fmt.Sprintf("%s (code %d)", name, code)
	}
	return fmt.Sprintf("%s (code %d, hresult 0x%08X)", name, code, uint32(hresult))
}

// wuaSelect picks the search results whose UpdateID appears in ids,
// preserving the order the caller asked for, and reports which ids the
// search did not return. Splitting this out of the COM loop is what lets it
// be tested.
func wuaSelect(found []wuaFields, ids []string) (matched []wuaFields, missing []string) {
	byID := make(map[string]wuaFields, len(found))
	for _, f := range found {
		byID[strings.ToLower(f.UpdateID)] = f
	}
	for _, id := range ids {
		f, ok := byID[strings.ToLower(strings.TrimSpace(id))]
		if !ok {
			missing = append(missing, id)
			continue
		}
		matched = append(matched, f)
	}
	return matched, missing
}
