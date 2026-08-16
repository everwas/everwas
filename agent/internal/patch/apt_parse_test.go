package patch

import (
	"reflect"
	"testing"
)

// aptSimulateFixture is real `apt-get -s -o Debug::NoLocking=1 dist-upgrade`
// output from a Debian 11 host, including the Conf and Remv lines the
// parser must ignore and the trailing "[]" apt appends to some Inst lines.
const aptSimulateFixture = `NOTE: This is only a simulation!
      apt-get needs root privileges for real execution.
      Keep also in mind that locking is deactivated,
      so don't depend on the relevance to the real current situation!
Reading package lists...
Building dependency tree...
Reading state information...
Calculating upgrade...
The following packages will be upgraded:
  base-files libc-bin libc6 linux-image-amd64
5 upgraded, 1 newly installed, 1 to remove and 0 not upgraded.
Remv oldpkg [1.0-1]
Inst base-files [11.1+deb11u5] (11.1+deb11u6 Debian:11.7/stable [amd64])
Conf base-files (11.1+deb11u6 Debian:11.7/stable [amd64])
Inst libc6 [2.31-13+deb11u3] (2.31-13+deb11u5 Debian-Security:11/oldstable [amd64]) []
Inst libc-bin [2.31-13+deb11u3] (2.31-13+deb11u5 Debian-Security:11/oldstable [amd64])
Inst linux-image-amd64 [5.10.162-1] (5.10.179-1 Debian:11.7/stable, Debian-Security:11/oldstable [amd64])
Inst newthing (1.0-1 Ubuntu:22.04/jammy-security [amd64])
Inst libc6:i386 [2.31-13] (2.31-13+deb11u5 Debian:11/stable [i386])
Conf linux-image-amd64 (5.10.179-1 Debian:11.7/stable [amd64])
`

func TestParseAptSimulate(t *testing.T) {
	got := parseAptSimulate(aptSimulateFixture)
	want := []Update{
		{
			ID: "base-files=11.1+deb11u6", Title: "base-files 11.1+deb11u5 to 11.1+deb11u6",
			Kind: KindOther, Severity: SeverityUnknown,
		},
		{
			ID: "libc6=2.31-13+deb11u5", Title: "libc6 2.31-13+deb11u3 to 2.31-13+deb11u5",
			Kind: KindSecurity, Severity: SeverityUnknown, RebootLikely: true,
		},
		{
			ID: "libc-bin=2.31-13+deb11u5", Title: "libc-bin 2.31-13+deb11u3 to 2.31-13+deb11u5",
			Kind: KindSecurity, Severity: SeverityUnknown, RebootLikely: true,
		},
		{
			ID:    "linux-image-amd64=5.10.179-1",
			Title: "linux-image-amd64 5.10.162-1 to 5.10.179-1",
			Kind:  KindSecurity, Severity: SeverityUnknown, RebootLikely: true,
		},
		{
			ID: "newthing=1.0-1", Title: "newthing 1.0-1",
			Kind: KindSecurity, Severity: SeverityUnknown,
		},
		{
			ID: "libc6:i386=2.31-13+deb11u5", Title: "libc6:i386 2.31-13 to 2.31-13+deb11u5",
			Kind: KindOther, Severity: SeverityUnknown, RebootLikely: true,
		},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d updates, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("update %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestParseAptInstLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want aptInst
		ok   bool
	}{
		{
			name: "upgrade with old version",
			line: "Inst libc6 [2.31-13] (2.31-13+deb11u5 Debian-Security:11/oldstable [amd64])",
			want: aptInst{Name: "libc6", OldVersion: "2.31-13", NewVersion: "2.31-13+deb11u5",
				Origins: "Debian-Security:11/oldstable", Arch: "amd64"},
			ok: true,
		},
		{
			name: "new install without old version",
			line: "Inst newpkg (1.0-1 Ubuntu:22.04/jammy [amd64]) []",
			want: aptInst{Name: "newpkg", NewVersion: "1.0-1",
				Origins: "Ubuntu:22.04/jammy", Arch: "amd64"},
			ok: true,
		},
		{
			name: "multiple origins",
			line: "Inst linux-image-amd64 [5.10.162-1] (5.10.179-1 Debian:11.7/stable, Debian-Security:11/oldstable [amd64])",
			want: aptInst{Name: "linux-image-amd64", OldVersion: "5.10.162-1", NewVersion: "5.10.179-1",
				Origins: "Debian:11.7/stable, Debian-Security:11/oldstable", Arch: "amd64"},
			ok: true,
		},
		{
			name: "multiarch qualifier",
			line: "Inst libc6:i386 [2.31-13] (2.31-13+deb11u5 Debian:11/stable [i386])",
			want: aptInst{Name: "libc6:i386", OldVersion: "2.31-13", NewVersion: "2.31-13+deb11u5",
				Origins: "Debian:11/stable", Arch: "i386"},
			ok: true,
		},
		{
			name: "local repo with no arch bracket",
			line: "Inst mypkg [1.0] (2.0 my-local-repo)",
			want: aptInst{Name: "mypkg", OldVersion: "1.0", NewVersion: "2.0", Origins: "my-local-repo"},
			ok:   true,
		},
		{name: "conf line", line: "Conf base-files (11.1 Debian:11/stable [amd64])"},
		{name: "remv line", line: "Remv oldpkg [1.0-1]"},
		{name: "prose", line: "The following packages will be upgraded:"},
		{name: "empty", line: ""},
		{name: "truncated mid line", line: "Inst libc6 [2.31-13] (2.31-13+deb11u5 Debian"},
		{name: "no version in parens", line: "Inst libc6 ()"},
		{name: "unterminated old version", line: "Inst libc6 [2.31-13 (2.0 Debian [amd64])"},
		{name: "name only", line: "Inst libc6"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAptInstLine(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestAptIsSecurityOrigin(t *testing.T) {
	tests := []struct {
		origins string
		want    bool
	}{
		{"Debian-Security:11/oldstable", true},
		{"Ubuntu:22.04/jammy-security", true},
		{"Debian:11.7/stable, Debian-Security:11/oldstable", true},
		{"debian-security:12/stable", true},
		{"Debian:11.7/stable", false},
		{"Ubuntu:22.04/jammy-updates", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := aptIsSecurityOrigin(tc.origins); got != tc.want {
			t.Errorf("aptIsSecurityOrigin(%q) = %v, want %v", tc.origins, got, tc.want)
		}
	}
}

func TestAptRebootLikely(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"linux-image-amd64", true},
		{"linux-image-5.10.0-21-amd64", true},
		{"libc6", true},
		{"libc6:i386", true},
		{"systemd", true},
		{"grub-efi-amd64", true},
		{"vim", false},
		{"linuxbrew-wrapper", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := aptRebootLikely(tc.name); got != tc.want {
			t.Errorf("aptRebootLikely(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAptErrorClassification(t *testing.T) {
	transient := []string{
		"Err:1 http://deb.debian.org/debian bullseye InRelease\n  Temporary failure resolving 'deb.debian.org'",
		"E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/jammy/InRelease  503  Service Unavailable",
		"Err:3 https://mirror.example/debian bookworm/main amd64 Packages\n  Hash Sum mismatch",
	}
	for _, text := range transient {
		if !aptIsTransientUpdateError(text) {
			t.Errorf("expected transient: %q", text)
		}
	}
	permanent := []string{
		"E: The repository 'http://example.invalid bookworm Release' does not have a Release file.",
		"E: Could not open lock file /var/lib/apt/lists/lock - open (13: Permission denied)",
		"",
	}
	for _, text := range permanent {
		if aptIsTransientUpdateError(text) {
			t.Errorf("expected permanent: %q", text)
		}
	}

	locked := []string{
		"E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 2107 (unattended-upgr)",
		"E: Unable to acquire the dpkg frontend lock (/var/lib/dpkg/lock-frontend), is another process using it?",
		"dpkg: error: dpkg frontend lock was locked by another process",
	}
	for _, text := range locked {
		if !aptIsLockContention(text) {
			t.Errorf("expected lock contention: %q", text)
		}
	}
	if aptIsLockContention("E: Unable to locate package doesnotexist") {
		t.Error("package-not-found must not read as lock contention")
	}
}

func TestParseDpkgVersions(t *testing.T) {
	out := "base-files\t11.1+deb11u6\nlibc6\t2.31-13+deb11u5\nbroken-line-without-tab\n\t\n"
	got := parseDpkgVersions(out)
	want := map[string]string{
		"base-files": "11.1+deb11u6",
		"libc6":      "2.31-13+deb11u5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAptProgressPhase(t *testing.T) {
	tests := []struct {
		line  string
		phase string
		ok    bool
	}{
		{"Get:1 http://deb.debian.org/debian bullseye/main amd64 libc6 amd64 2.31-13 [2,806 kB]", PhaseDownload, true},
		{"Preparing to unpack .../libc6_2.31-13+deb11u5_amd64.deb ...", PhaseInstall, true},
		{"Unpacking libc6:amd64 (2.31-13+deb11u5) over (2.31-13+deb11u3) ...", PhaseInstall, true},
		{"Setting up libc6:amd64 (2.31-13+deb11u5) ...", PhaseInstall, true},
		{"Reading package lists...", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		phase, ok := aptProgressPhase(tc.line)
		if phase != tc.phase || ok != tc.ok {
			t.Errorf("aptProgressPhase(%q) = (%q, %v), want (%q, %v)", tc.line, phase, ok, tc.phase, tc.ok)
		}
	}
}

func TestSplitPkgVersionID(t *testing.T) {
	tests := []struct {
		id      string
		name    string
		version string
		ok      bool
	}{
		{"libc6=2.31-13", "libc6", "2.31-13", true},
		{"libc6:i386=2.31-13", "libc6:i386", "2.31-13", true},
		{"libc6", "", "", false},
		{"=2.31", "", "", false},
		{"libc6=", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		name, version, ok := splitPkgVersionID(tc.id)
		if name != tc.name || version != tc.version || ok != tc.ok {
			t.Errorf("splitPkgVersionID(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.id, name, version, ok, tc.name, tc.version, tc.ok)
		}
	}
}
