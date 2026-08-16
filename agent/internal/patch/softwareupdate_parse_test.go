package patch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

// TestParseSoftwareUpdateList runs the parser against real
// `softwareupdate --list` output from four macOS releases. The block format
// drifted across all four (size suffix K to KiB, the banner disappearing in
// 14, Deferred appearing in 15), which is why each is pinned here.
func TestParseSoftwareUpdateList(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		host    macOSHost
		want    []Update
	}{
		{
			name:    "macos 12 monterey on intel",
			fixture: "softwareupdate_macos12.txt",
			host:    macOSHost{AppleSilicon: false, MajorVersion: 12},
			want: []Update{
				{
					ID: "Safari15.6.1MontereyAuto-15.6.1", Title: "Safari 15.6.1",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 89747 * 1024,
				},
				{
					ID: "macOS Monterey 12.6.1-21G217", Title: "macOS Monterey 12.6.1",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 2914283 * 1024,
					RebootLikely: true,
				},
				{
					ID: "XProtectPlistConfigData_10_15-2151", Title: "XProtectPlistConfigData 2151",
					Kind: KindDefinition, Severity: SeverityUnknown, SizeBytes: 5427 * 1024,
				},
				{
					ID: "Command Line Tools for Xcode-14.0", Title: "Command Line Tools for Xcode 14.0",
					Kind: KindOther, Severity: SeverityUnknown, SizeBytes: 686060 * 1024,
				},
			},
		},
		{
			name:    "macos 13 ventura on apple silicon",
			fixture: "softwareupdate_macos13.txt",
			host:    macOSHost{AppleSilicon: true, MajorVersion: 13},
			want: []Update{
				{
					ID: "macOS Ventura 13.6.6-22G630", Title: "macOS Ventura 13.6.6",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 3627470 * 1024,
					RebootLikely: true, Unsupported: true, Detail: detailAppleSilicon,
				},
				{
					ID: "Safari16.6VenturaAuto-16.6", Title: "Safari 16.6",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 128173 * 1024,
				},
				{
					ID: "MRTConfigData_10_15-1.93", Title: "MRTConfigData 1.93",
					Kind: KindDefinition, Severity: SeverityUnknown, SizeBytes: 6963 * 1024,
				},
			},
		},
		{
			name:    "macos 14 sonoma on apple silicon with an upgrade offered",
			fixture: "softwareupdate_macos14.txt",
			host:    macOSHost{AppleSilicon: true, MajorVersion: 14},
			want: []Update{
				{
					ID: "macOS Sonoma 14.5-23F79", Title: "macOS Sonoma 14.5",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 6081868 * 1024,
					RebootLikely: true, Unsupported: true, Detail: detailAppleSilicon,
				},
				{
					ID: "Safari17.5SonomaAuto-17.5", Title: "Safari 17.5",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 128173 * 1024,
				},
				{
					ID: "macOS Sequoia 15.0-24A335", Title: "macOS Sequoia 15.0",
					Kind: KindFeature, Severity: SeverityUnknown, SizeBytes: 7213268 * 1024,
					RebootLikely: true, Unsupported: true, Detail: detailMajorUpgrade,
				},
			},
		},
		{
			name:    "macos 15 sequoia on intel",
			fixture: "softwareupdate_macos15.txt",
			host:    macOSHost{AppleSilicon: false, MajorVersion: 15},
			want: []Update{
				{
					ID: "macOS Sequoia 15.5-24F74", Title: "macOS Sequoia 15.5",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 7213268 * 1024,
					RebootLikely: true,
				},
				{
					ID: "Safari18.5Sequoia-18.5", Title: "Safari 18.5",
					Kind: KindSecurity, Severity: SeverityUnknown, SizeBytes: 132048 * 1024,
				},
				{
					ID: "Command Line Tools for Xcode-16.3", Title: "Command Line Tools for Xcode 16.3",
					Kind: KindOther, Severity: SeverityUnknown, SizeBytes: 725238 * 1024,
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, degraded := parseSoftwareUpdateList(loadFixture(t, tc.fixture), tc.host)
			if len(degraded) != 0 {
				t.Errorf("unexpected degraded blocks: %v", degraded)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %d updates, want %d", len(got), len(tc.want))
				for i := range got {
					if i < len(tc.want) && !reflect.DeepEqual(got[i], tc.want[i]) {
						t.Errorf("update %d:\n got %+v\nwant %+v", i, got[i], tc.want[i])
					}
				}
			}
		})
	}
}

// TestParseSoftwareUpdateListDegrades proves a truncated or nameless block
// costs us that block and nothing else.
func TestParseSoftwareUpdateListDegrades(t *testing.T) {
	got, degraded := parseSoftwareUpdateList(
		loadFixture(t, "softwareupdate_malformed.txt"),
		macOSHost{AppleSilicon: true, MajorVersion: 15},
	)
	wantIDs := []string{"macOS Sequoia 15.5-24F74", "WeirdSize-2.0", "Safari18.5Sequoia-18.5"}
	if len(got) != len(wantIDs) {
		t.Fatalf("parsed %d updates, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("update %d id = %q, want %q", i, got[i].ID, id)
		}
	}
	if got[1].SizeBytes != 0 {
		t.Errorf("an unreadable size must degrade to 0, got %d", got[1].SizeBytes)
	}
	wantDegraded := []string{"TruncatedBlock-1.0", "(empty label)"}
	if !reflect.DeepEqual(degraded, wantDegraded) {
		t.Errorf("degraded = %v, want %v", degraded, wantDegraded)
	}
}

func TestParseSoftwareUpdateListEmpty(t *testing.T) {
	tests := []struct{ name, out string }{
		{"no updates", "Software Update Tool\n\nFinding available software\nNo new software available.\n"},
		{"empty output", ""},
		{"banner only", "Software Update Tool\n"},
		{"label marker with nothing after it", "* Label:\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseSoftwareUpdateList(tc.out, macOSHost{MajorVersion: 15})
			if len(got) != 0 {
				t.Errorf("got %d updates, want none: %+v", len(got), got)
			}
		})
	}
}

func TestParseSoftwareUpdateAttrs(t *testing.T) {
	tests := []struct {
		name string
		line string
		want map[string]string
	}{
		{
			name: "macos 15 with deferred",
			line: "Title: Safari, Version: 18.5, Size: 132048KiB, Recommended: YES, Deferred: NO,",
			want: map[string]string{"Title": "Safari", "Version": "18.5", "Size": "132048KiB",
				"Recommended": "YES", "Deferred": "NO"},
		},
		{
			name: "title containing a comma",
			line: "Title: Update for Foo, Bar and Baz, Version: 1.0, Size: 10KiB,",
			want: map[string]string{"Title": "Update for Foo, Bar and Baz", "Version": "1.0", "Size": "10KiB"},
		},
		{
			name: "trailing action",
			line: "Title: macOS Sonoma, Version: 14.5, Size: 6081868KiB, Recommended: YES, Action: restart,",
			want: map[string]string{"Title": "macOS Sonoma", "Version": "14.5", "Size": "6081868KiB",
				"Recommended": "YES", "Action": "restart"},
		},
		{name: "empty", line: "", want: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSoftwareUpdateAttrs(tc.line); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestParseSoftwareUpdateSize(t *testing.T) {
	tests := []struct {
		field string
		want  int64
	}{
		{"89747K", 89747 * 1024},
		{"6081868KiB", 6081868 * 1024},
		{"12MiB", 12 * 1024 * 1024},
		{"2GB", 2 * 1024 * 1024 * 1024},
		{"1024B", 1024},
		{"512", 512},
		{"1.5MiB", 1572864},
		{"notanumber", 0},
		{"", 0},
		{"-", 0},
	}
	for _, tc := range tests {
		if got := parseSoftwareUpdateSize(tc.field); got != tc.want {
			t.Errorf("parseSoftwareUpdateSize(%q) = %d, want %d", tc.field, got, tc.want)
		}
	}
}

func TestIsMajorMacOSUpgrade(t *testing.T) {
	tests := []struct {
		version string
		running int
		want    bool
	}{
		{"15.5", 15, false},
		{"15.0", 14, true},
		{"14.5", 15, false},
		{"", 15, true},
		{"notaversion", 15, true},
		{"15.5", 0, true}, // unknown running version means do not touch it
	}
	for _, tc := range tests {
		if got := isMajorMacOSUpgrade(tc.version, tc.running); got != tc.want {
			t.Errorf("isMajorMacOSUpgrade(%q, %d) = %v, want %v",
				tc.version, tc.running, got, tc.want)
		}
	}
}

func TestParseSWVersMajor(t *testing.T) {
	tests := []struct {
		out  string
		want int
	}{
		{"15.5\n", 15},
		{"12.6.1\n", 12},
		{"26\n", 26},
		{"", 0},
		{"garbage\n", 0},
	}
	for _, tc := range tests {
		if got := parseSWVersMajor(tc.out); got != tc.want {
			t.Errorf("parseSWVersMajor(%q) = %d, want %d", tc.out, got, tc.want)
		}
	}
}

func TestSoftwareUpdateInstallFailed(t *testing.T) {
	failures := []string{
		"softwareupdate: Failed to install macOS Sequoia 15.5",
		"Update could not be installed because it requires authentication",
		"No such update: BogusLabel-1.0",
	}
	for _, text := range failures {
		if !softwareUpdateInstallFailed(text) {
			t.Errorf("expected failure text: %q", text)
		}
	}
	if softwareUpdateInstallFailed("Downloading Safari 18.5\nInstalling: 100%\nDone.") {
		t.Error("a clean install must not read as a failure")
	}
}
