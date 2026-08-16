package patch

import (
	"reflect"
	"strings"
	"testing"
)

// The WUA backend cannot be exercised on a non-Windows builder, so the
// COM plumbing is kept in wua_windows.go and everything that decides what
// an update MEANS lives in wua_map.go and is tested here against the field
// values Windows actually returns.

func TestMapMsrcSeverity(t *testing.T) {
	tests := []struct{ msrc, want string }{
		{"Critical", SeverityCritical},
		{"Important", SeverityImportant},
		{"Moderate", SeverityModerate},
		{"Low", SeverityLow},
		{"critical", SeverityCritical},
		{"  Important  ", SeverityImportant},
		{"Unspecified", SeverityUnknown},
		{"", SeverityUnknown},
		{"Sev5", SeverityUnknown},
	}
	for _, tc := range tests {
		if got := mapMsrcSeverity(tc.msrc); got != tc.want {
			t.Errorf("mapMsrcSeverity(%q) = %q, want %q", tc.msrc, got, tc.want)
		}
	}
}

func TestWUAKind(t *testing.T) {
	tests := []struct {
		name       string
		categories []string
		title      string
		want       string
	}{
		{
			name:       "defender definitions are definitions, not security",
			categories: []string{"Definition Updates", "Microsoft Defender Antivirus"},
			title:      "Security Intelligence Update for Microsoft Defender Antivirus - KB2267602",
			want:       KindDefinition,
		},
		{
			name:       "cumulative security update",
			categories: []string{"Security Updates", "Windows 11"},
			title:      "2026-01 Cumulative Update for Windows 11 Version 24H2 (KB5034123)",
			want:       KindSecurity,
		},
		{
			name:       "critical updates count as security",
			categories: []string{"Critical Updates"},
			title:      "Update for Windows",
			want:       KindSecurity,
		},
		{
			name:       "feature pack",
			categories: []string{"Feature Packs"},
			title:      "Feature update to Windows 11",
			want:       KindFeature,
		},
		{
			name:       "no categories falls back to the title",
			categories: nil,
			title:      "2026-01 Security Update for Windows Server 2022 (KB5034129)",
			want:       KindSecurity,
		},
		{
			name:       "no categories and a plain title",
			categories: nil,
			title:      "Update for Microsoft Edge",
			want:       KindOther,
		},
		{
			name:       "servicing stack update",
			categories: []string{"Updates"},
			title:      "Servicing Stack Update for Windows Server 2022 (KB5034127)",
			want:       KindOther,
		},
		{name: "nothing at all", want: KindOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wuaKind(tc.categories, tc.title); got != tc.want {
				t.Errorf("wuaKind(%v, %q) = %q, want %q", tc.categories, tc.title, got, tc.want)
			}
		})
	}
}

func TestWUARebootLikely(t *testing.T) {
	tests := []struct {
		behavior int64
		want     bool
	}{
		{wuaNeverReboots, false},
		{wuaAlwaysRequiresReboot, true},
		{wuaCanRequestReboot, true},
		{99, false},
	}
	for _, tc := range tests {
		if got := wuaRebootLikely(tc.behavior); got != tc.want {
			t.Errorf("wuaRebootLikely(%d) = %v, want %v", tc.behavior, got, tc.want)
		}
	}
}

func TestNormalizeKBIDs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "bare numbers get the prefix", in: []string{"5034123", "2267602"},
			want: []string{"KB5034123", "KB2267602"}},
		{name: "already prefixed", in: []string{"KB5034123"}, want: []string{"KB5034123"}},
		{name: "lowercase prefix is normalised", in: []string{"kb5034123"}, want: []string{"KB5034123"}},
		{name: "duplicates collapse", in: []string{"5034123", "KB5034123"}, want: []string{"KB5034123"}},
		{name: "blanks dropped", in: []string{"", "   "}, want: nil},
		{name: "nil in nil out", in: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeKBIDs(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalizeKBIDs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestWUAUpdateFromFields(t *testing.T) {
	tests := []struct {
		name   string
		fields wuaFields
		want   Update
	}{
		{
			name: "cumulative security update",
			fields: wuaFields{
				UpdateID:        "7d5a4c1e-9c8b-4b2a-9e4f-1a2b3c4d5e6f",
				Title:           "2026-01 Cumulative Update for Windows 11 Version 24H2 (KB5034123)",
				MsrcSeverity:    "Critical",
				KBIDs:           []string{"5034123"},
				Categories:      []string{"Security Updates", "Windows 11"},
				MaxDownloadSize: 734003200,
				RebootBehavior:  wuaAlwaysRequiresReboot,
				EulaAccepted:    true,
			},
			want: Update{
				ID:       "7d5a4c1e-9c8b-4b2a-9e4f-1a2b3c4d5e6f",
				Title:    "2026-01 Cumulative Update for Windows 11 Version 24H2 (KB5034123)",
				Kind:     KindSecurity,
				KBIDs:    []string{"KB5034123"},
				Severity: SeverityCritical,

				SizeBytes:    734003200,
				RebootLikely: true,
			},
		},
		{
			name: "defender definition with no severity",
			fields: wuaFields{
				UpdateID:        "00000000-0000-0000-0000-000000000001",
				Title:           "  Security Intelligence Update for Microsoft Defender Antivirus - KB2267602  ",
				Categories:      []string{"Definition Updates"},
				MaxDownloadSize: 120586240,
				RebootBehavior:  wuaNeverReboots,
			},
			want: Update{
				ID:        "00000000-0000-0000-0000-000000000001",
				Title:     "Security Intelligence Update for Microsoft Defender Antivirus - KB2267602",
				Kind:      KindDefinition,
				Severity:  SeverityUnknown,
				SizeBytes: 120586240,
			},
		},
		{
			name: "an update that would prompt is reported but not installable",
			fields: wuaFields{
				UpdateID:            "00000000-0000-0000-0000-000000000002",
				Title:               "Update that asks a question",
				RebootBehavior:      wuaCanRequestReboot,
				CanRequestUserInput: true,
			},
			want: Update{
				ID:           "00000000-0000-0000-0000-000000000002",
				Title:        "Update that asks a question",
				Kind:         KindOther,
				Severity:     SeverityUnknown,
				RebootLikely: true,
				Unsupported:  true,
				Detail:       detailUserInput,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wuaUpdateFromFields(tc.fields); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestWUAResultOK(t *testing.T) {
	tests := []struct {
		code int64
		want bool
	}{
		{wuaResultSucceeded, true},
		{wuaResultSucceededWithErrors, true},
		{wuaResultFailed, false},
		{wuaResultAborted, false},
		{wuaResultNotStarted, false},
		{wuaResultInProgress, false},
	}
	for _, tc := range tests {
		if got := wuaResultOK(tc.code); got != tc.want {
			t.Errorf("wuaResultOK(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestWUAResultText(t *testing.T) {
	got := wuaResultText(wuaResultFailed, 0x80240022)
	for _, want := range []string{"failed", "code 4", "0x80240022"} {
		if !strings.Contains(got, want) {
			t.Errorf("wuaResultText = %q, missing %q", got, want)
		}
	}
	if got := wuaResultText(wuaResultSucceeded, 0); !strings.Contains(got, "succeeded") {
		t.Errorf("wuaResultText = %q, want it to say succeeded", got)
	}
}

func TestWUASelect(t *testing.T) {
	found := []wuaFields{
		{UpdateID: "AAAA-1111"},
		{UpdateID: "BBBB-2222"},
		{UpdateID: "CCCC-3333"},
	}
	matched, missing := wuaSelect(found, []string{"bbbb-2222", " AAAA-1111 ", "DDDD-4444"})
	wantMatched := []string{"BBBB-2222", "AAAA-1111"}
	if len(matched) != len(wantMatched) {
		t.Fatalf("matched %d, want %d", len(matched), len(wantMatched))
	}
	for i, want := range wantMatched {
		if matched[i].UpdateID != want {
			t.Errorf("matched[%d] = %q, want %q", i, matched[i].UpdateID, want)
		}
	}
	if !reflect.DeepEqual(missing, []string{"DDDD-4444"}) {
		t.Errorf("missing = %v, want [DDDD-4444]", missing)
	}
}

func TestWUASearchCriteria(t *testing.T) {
	// Drivers must stay out of the search: pushing a driver silently is how
	// an RMM bricks a fleet of laptops.
	for _, want := range []string{"IsInstalled=0", "IsHidden=0", "Type='Software'"} {
		if !strings.Contains(wuaSearchCriteria, want) {
			t.Errorf("search criteria %q is missing %q", wuaSearchCriteria, want)
		}
	}
}
