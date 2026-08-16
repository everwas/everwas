package patch

import (
	"reflect"
	"testing"
)

// dnfCheckUpdateFixture is `dnf -q check-update` output from a RHEL 9 host.
// It includes a package name long enough that dnf wraps the remaining two
// columns onto the next line, and the Obsoleting Packages section that must
// not be reported as available updates.
const dnfCheckUpdateFixture = `kernel.x86_64                          5.14.0-284.11.1.el9_2          baseos
kernel-core.x86_64                     5.14.0-284.11.1.el9_2          baseos
openssl-libs.x86_64                    1:3.0.7-6.el9_2                baseos
python3-dnf-plugins-core-extra-long.noarch
                                       4.3.0-5.el9                    appstream
vim-minimal.x86_64                     2:8.2.2637-20.el9_1            appstream

Obsoleting Packages
old-thing.noarch                       2.0-1.el9                      appstream
`

// dnfUpdateinfoFixture mixes the column layouts different dnf releases
// print, which is exactly what the parser has to survive.
const dnfUpdateinfoFixture = `RHSA-2023:3106 Important/Sec.  kernel-5.14.0-284.11.1.el9_2.x86_64
RHSA-2023:3106 Important/Sec.  kernel-core-5.14.0-284.11.1.el9_2.x86_64
RHSA-2023:2523 Moderate/Sec.   openssl-libs-1:3.0.7-6.el9_2.x86_64
FEDORA-2023-abc123 CVE-2023-1234 Critical/Sec. vim-minimal-2:8.2.2637-20.el9_1.x86_64
RHBA-2023:1111 bugfix          python3-dnf-plugins-core-extra-long-4.3.0-5.el9.noarch
Updateinfo list done
`

func TestParseDNFCheckUpdate(t *testing.T) {
	got := parseDNFCheckUpdate(dnfCheckUpdateFixture)
	want := []dnfEntry{
		{Name: "kernel", Arch: "x86_64", EVR: "5.14.0-284.11.1.el9_2", Repo: "baseos"},
		{Name: "kernel-core", Arch: "x86_64", EVR: "5.14.0-284.11.1.el9_2", Repo: "baseos"},
		{Name: "openssl-libs", Arch: "x86_64", EVR: "1:3.0.7-6.el9_2", Repo: "baseos"},
		{Name: "python3-dnf-plugins-core-extra-long", Arch: "noarch", EVR: "4.3.0-5.el9", Repo: "appstream"},
		{Name: "vim-minimal", Arch: "x86_64", EVR: "2:8.2.2637-20.el9_1", Repo: "appstream"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestParseDNFCheckUpdateMalformed(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int
	}{
		{"empty", "", 0},
		{"only a header", "Last metadata expiration check: 0:12:34 ago on Tue.\n", 0},
		{"truncated mid line", "kernel.x86_64   5.14.0", 0},
		{"dangling wrapped name", "some-very-long-name.noarch\n", 0},
		{"blank lines only", "\n\n\n", 0},
		{"obsoleting only", "Obsoleting Packages\nfoo.noarch 1.0 repo\n", 0},
		{"extra columns tolerated", "kernel.x86_64 5.14.0-1.el9 baseos extra\n", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(parseDNFCheckUpdate(tc.out)); got != tc.want {
				t.Errorf("parsed %d entries, want %d", got, tc.want)
			}
		})
	}
}

func TestSplitNameArch(t *testing.T) {
	tests := []struct{ token, name, arch string }{
		{"kernel.x86_64", "kernel", "x86_64"},
		{"python3.11-libs.x86_64", "python3.11-libs", "x86_64"},
		{"python3.11", "python3.11", ""},
		{"nothing", "nothing", ""},
		{"foo.noarch", "foo", "noarch"},
		{"foo.aarch64", "foo", "aarch64"},
		{"", "", ""},
	}
	for _, tc := range tests {
		name, arch := splitNameArch(tc.token)
		if name != tc.name || arch != tc.arch {
			t.Errorf("splitNameArch(%q) = (%q, %q), want (%q, %q)",
				tc.token, name, arch, tc.name, tc.arch)
		}
	}
}

func TestDNFInstallSpec(t *testing.T) {
	tests := []struct {
		id      string
		want    string
		wantErr bool
	}{
		{id: "kernel.x86_64=5.14.0-284.11.1.el9_2", want: "kernel-5.14.0-284.11.1.el9_2.x86_64"},
		{id: "openssl-libs.x86_64=1:3.0.7-6.el9_2", want: "openssl-libs-1:3.0.7-6.el9_2.x86_64"},
		{id: "noarchpkg=1.0-1", want: "noarchpkg-1.0-1"},
		{id: "kernel.x86_64", wantErr: true},
		{id: "=1.0", wantErr: true},
		{id: "kernel.x86_64=", wantErr: true},
		{id: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := dnfInstallSpec(tc.id)
		if (err != nil) != tc.wantErr {
			t.Errorf("dnfInstallSpec(%q) err = %v, wantErr %v", tc.id, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("dnfInstallSpec(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestParseDNFUpdateinfo(t *testing.T) {
	got := parseDNFUpdateinfo(dnfUpdateinfoFixture)
	want := map[string]dnfAdvisory{
		"kernel-5.14.0-284.11.1.el9_2.x86_64":      {Severity: SeverityImportant, Security: true},
		"kernel-core-5.14.0-284.11.1.el9_2.x86_64": {Severity: SeverityImportant, Security: true},
		"openssl-libs-1:3.0.7-6.el9_2.x86_64":      {Severity: SeverityModerate, Security: true},
		"vim-minimal-2:8.2.2637-20.el9_1.x86_64":   {Severity: SeverityCritical, Security: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestParseDNFUpdateinfoWorstSeverityWins(t *testing.T) {
	out := "RHSA-1 Low/Sec. kernel-1.el9.x86_64\nRHSA-2 Critical/Sec. kernel-1.el9.x86_64\nRHSA-3 Moderate/Sec. kernel-1.el9.x86_64\n"
	got := parseDNFUpdateinfo(out)
	if got["kernel-1.el9.x86_64"].Severity != SeverityCritical {
		t.Errorf("severity = %q, want %q", got["kernel-1.el9.x86_64"].Severity, SeverityCritical)
	}
}

func TestDNFUpdates(t *testing.T) {
	entries := parseDNFCheckUpdate(dnfCheckUpdateFixture)
	advisories := parseDNFUpdateinfo(dnfUpdateinfoFixture)
	got := dnfUpdates(entries, advisories)

	want := []Update{
		{ID: "kernel.x86_64=5.14.0-284.11.1.el9_2", Title: "kernel-5.14.0-284.11.1.el9_2.x86_64",
			Kind: KindSecurity, Severity: SeverityImportant, RebootLikely: true},
		{ID: "kernel-core.x86_64=5.14.0-284.11.1.el9_2", Title: "kernel-core-5.14.0-284.11.1.el9_2.x86_64",
			Kind: KindSecurity, Severity: SeverityImportant, RebootLikely: true},
		{ID: "openssl-libs.x86_64=1:3.0.7-6.el9_2", Title: "openssl-libs-1:3.0.7-6.el9_2.x86_64",
			Kind: KindSecurity, Severity: SeverityModerate, RebootLikely: true},
		{ID: "python3-dnf-plugins-core-extra-long.noarch=4.3.0-5.el9",
			Title: "python3-dnf-plugins-core-extra-long-4.3.0-5.el9.noarch",
			Kind:  KindOther, Severity: SeverityUnknown},
		{ID: "vim-minimal.x86_64=2:8.2.2637-20.el9_1", Title: "vim-minimal-2:8.2.2637-20.el9_1.x86_64",
			Kind: KindSecurity, Severity: SeverityCritical},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestDNFRebootLikely(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"kernel", true},
		{"kernel-core", true},
		{"kernel-modules-extra", true},
		{"glibc", true},
		{"openssl-libs", true},
		{"systemd-udev", true},
		{"vim-minimal", false},
		{"kernelshark", false}, // prefix match must respect the hyphen
		{"", false},
	}
	for _, tc := range tests {
		if got := dnfRebootLikely(tc.name); got != tc.want {
			t.Errorf("dnfRebootLikely(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStripZeroEpoch(t *testing.T) {
	tests := []struct{ in, want string }{
		{"pkg-0:1.0-1.el9.x86_64", "pkg-1.0-1.el9.x86_64"},
		{"pkg-1:1.0-1.el9.x86_64", "pkg-1:1.0-1.el9.x86_64"},
		{"pkg-1.0-1.el9.x86_64", "pkg-1.0-1.el9.x86_64"},
	}
	for _, tc := range tests {
		if got := stripZeroEpoch(tc.in); got != tc.want {
			t.Errorf("stripZeroEpoch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDNFPluginMissing(t *testing.T) {
	if !dnfPluginMissing("No such command: needs-restarting. Please use /usr/bin/dnf --help") {
		t.Error("expected the missing-plugin message to be recognised")
	}
	if dnfPluginMissing("Core libraries are updated, reboot is required") {
		t.Error("a real reboot message must not read as a missing plugin")
	}
}

// TestDNFExitCodeContract pins the exit code meanings down in a test, since
// getting them wrong is the single most common way to break a dnf scan.
func TestDNFExitCodeContract(t *testing.T) {
	if dnfExitUpdates != 100 {
		t.Errorf("dnf check-update reports 100 when updates are available, got %d", dnfExitUpdates)
	}
	if dnfExitNoUpdates != 0 || dnfExitError != 1 {
		t.Errorf("dnf exit codes drifted: no-updates=%d error=%d", dnfExitNoUpdates, dnfExitError)
	}
}
