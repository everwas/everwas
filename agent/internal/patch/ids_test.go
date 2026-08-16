package patch

import (
	"errors"
	"strings"
	"testing"
)

// TestCheckPkgNameRejectsOptions is the regression for the defect that
// turned an update id into a pacman flag. "-Syu" reached the argv as a bare
// package name and became `pacman -S --noconfirm --needed -Syu`: a full
// unattended system upgrade, as root, on a host where nothing was approved.
func TestCheckPkgNameRejectsOptions(t *testing.T) {
	tests := []struct {
		name       string
		pkg        string
		allowUpper bool
		ok         bool
	}{
		{"ordinary name", "openssl", false, true},
		{"digits and dashes", "linux-lts", false, true},
		{"dots and pluses", "gcc-libs+ext.2", false, true},
		{"leading digit", "7zip", false, true},
		{"at sign", "node@20", false, true},
		{"rpm uppercase with allowUpper", "NetworkManager", true, true},

		{"pacman sync flag", "-Syu", false, false},
		{"pacman remove flag", "-Rns", false, false},
		{"long option", "--noconfirm", false, false},
		{"single dash", "-", false, false},
		{"apt option smuggled as a name", "--option", false, false},
		{"empty", "", false, false},
		{"rpm uppercase without allowUpper", "NetworkManager", false, false},
		{"space", "openssl bash", false, false},
		{"semicolon", "openssl;reboot", false, false},
		{"slash", "../../bin/sh", false, false},
		{"dollar", "$(id)", false, false},
		{"leading dot", ".hidden", false, false},
		{"too long", strings.Repeat("a", maxPkgTokenLen+1), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPkgName(tt.pkg, tt.allowUpper)
			if tt.ok && err != nil {
				t.Fatalf("checkPkgName(%q, %v) = %v, want nil", tt.pkg, tt.allowUpper, err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("checkPkgName(%q, %v) accepted an unusable name", tt.pkg, tt.allowUpper)
				}
				if !errors.Is(err, ErrBadUpdateID) {
					t.Fatalf("err = %v, want it to wrap ErrBadUpdateID", err)
				}
			}
		})
	}
}

func TestCheckPkgVersion(t *testing.T) {
	tests := []struct {
		version string
		ok      bool
	}{
		{"1.2.3", true},
		{"1:2.4.7-1ubuntu2.1", true},
		{"6.9.2.arch1-1", true},
		{"0:4.18.0-513.el8", true},
		{"1.0~rc1", true},
		{"1.0^20240101", true},
		{"", false},
		{"-1.0", false},
		{"1.0 --force", false},
		{"1.0;reboot", false},
		{"$(id)", false},
		{"Dpkg::Options::=--force-confnew", false},
		{strings.Repeat("1", maxPkgTokenLen+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			err := checkPkgVersion(tt.version)
			if tt.ok != (err == nil) {
				t.Fatalf("checkPkgVersion(%q) = %v, want ok=%v", tt.version, err, tt.ok)
			}
			if !tt.ok && !errors.Is(err, ErrBadUpdateID) {
				t.Fatalf("err = %v, want it to wrap ErrBadUpdateID", err)
			}
		})
	}
}

// TestPacmanInstallPlanRefusesFlagsAndUnofferedPackages covers the two ways
// a pacman install could previously do something nobody asked for: an id
// with no "=" fell through as a bare name, and no id was checked against
// what the host was actually offering.
func TestPacmanInstallPlanRefusesFlagsAndUnofferedPackages(t *testing.T) {
	offered := map[string]bool{"openssl": true, "linux": true}

	tests := []struct {
		name      string
		ids       []string
		wantNames []string
		wantFail  []string
	}{
		{
			name:      "an approved update goes through",
			ids:       []string{"openssl=3.3.1-1"},
			wantNames: []string{"openssl"},
		},
		{
			name:      "a pacman flag is refused, not run",
			ids:       []string{"-Syu"},
			wantNames: nil,
			wantFail:  []string{"-Syu"},
		},
		{
			name:      "a bare package name no longer round-trips",
			ids:       []string{"openssl"},
			wantNames: nil,
			wantFail:  []string{"openssl"},
		},
		{
			name:      "a flag dressed up with a version is refused",
			ids:       []string{"-Syu=1"},
			wantNames: nil,
			wantFail:  []string{"-Syu=1"},
		},
		{
			name:      "a package that is not pending is refused",
			ids:       []string{"bash=5.2-1"},
			wantNames: nil,
			wantFail:  []string{"bash=5.2-1"},
		},
		{
			name:      "the good ids in a mixed batch still install",
			ids:       []string{"-Syu", "openssl=3.3.1-1", "bash=5.2-1"},
			wantNames: []string{"openssl"},
			wantFail:  []string{"-Syu", "bash=5.2-1"},
		},
		{
			name:      "a stale version still installs, since pacman's version is advisory",
			ids:       []string{"linux=6.9.1-1"},
			wantNames: []string{"linux"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := newInstallResult()
			names, idByName := pacmanInstallPlan(tt.ids, offered, &res)
			assertStrings(t, "names", names, tt.wantNames)
			assertFailed(t, res, tt.wantFail)
			if len(names) != len(idByName) {
				t.Errorf("names %v and idByName %v disagree", names, idByName)
			}
		})
	}
}

// TestAptInstallPlanRejectsOptionsInIDs covers the id shape the review
// found: an id containing "=" round-trips into the argv, so
// "--option=Dpkg::Options::=--force-confnew" reached apt intact and inverted
// the conffile safety the code deliberately sets two lines earlier.
func TestAptInstallPlanRejectsOptionsInIDs(t *testing.T) {
	offered := map[string]bool{
		"openssl=3.0.13-0ubuntu3.4":       true,
		"login=1:4.13+dfsg1-4ubuntu3":     true,
		"linux-image-generic=6.8.0-40.40": true,
		// Present in the offered set on purpose: even if one of these ever
		// got scanned, it must still be refused on its shape alone.
		"--option=Dpkg::Options::=--force-confnew": true,
		"openssl=3.0.13-0ubuntu3.4 --force-yes":    true,
		"NetworkManager=1.46.0-1":                  true,
	}

	tests := []struct {
		name      string
		ids       []string
		wantSpecs []string
		wantFail  []string
	}{
		{
			name:      "an approved update goes through",
			ids:       []string{"openssl=3.0.13-0ubuntu3.4"},
			wantSpecs: []string{"openssl=3.0.13-0ubuntu3.4"},
		},
		{
			name:      "an epoch in the version is kept",
			ids:       []string{"login=1:4.13+dfsg1-4ubuntu3"},
			wantSpecs: []string{"login=1:4.13+dfsg1-4ubuntu3"},
		},
		{
			name:      "a dpkg option smuggled through the id is refused",
			ids:       []string{"--option=Dpkg::Options::=--force-confnew"},
			wantFail:  []string{"--option=Dpkg::Options::=--force-confnew"},
			wantSpecs: nil,
		},
		{
			name:      "a version carrying another argument is refused",
			ids:       []string{"openssl=3.0.13-0ubuntu3.4 --force-yes"},
			wantFail:  []string{"openssl=3.0.13-0ubuntu3.4 --force-yes"},
			wantSpecs: nil,
		},
		{
			name:      "an id with no version is refused",
			ids:       []string{"openssl"},
			wantFail:  []string{"openssl"},
			wantSpecs: nil,
		},
		{
			name:      "a package apt is not offering is refused",
			ids:       []string{"nginx=1.24.0-1"},
			wantFail:  []string{"nginx=1.24.0-1"},
			wantSpecs: nil,
		},
		{
			name:      "an uppercase name is not a Debian package",
			ids:       []string{"NetworkManager=1.46.0-1"},
			wantFail:  []string{"NetworkManager=1.46.0-1"},
			wantSpecs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := newInstallResult()
			specs, want, idByName := aptInstallPlan(tt.ids, offered, &res)
			assertStrings(t, "specs", specs, tt.wantSpecs)
			assertFailed(t, res, tt.wantFail)
			if len(want) != len(specs) || len(idByName) != len(specs) {
				t.Errorf("specs %v, want %v and idByName %v disagree", specs, want, idByName)
			}
		})
	}
}

func TestDNFInstallPlan(t *testing.T) {
	offered := map[string]bool{
		"bash.x86_64=0:5.1.8-9.el9":              true,
		"NetworkManager.x86_64=1:1.46.0-4.el9":   true,
		"python3.11-libs.x86_64=3.11.5-1.el9":    true,
		"kernel.x86_64=0:5.14.0-427.el9":         true,
		"--setopt.x86_64=install_weak_deps=True": true,
	}
	tests := []struct {
		name      string
		ids       []string
		wantSpecs []string
		wantFail  []string
	}{
		{
			name:      "an approved update goes through",
			ids:       []string{"bash.x86_64=0:5.1.8-9.el9"},
			wantSpecs: []string{"bash-0:5.1.8-9.el9.x86_64"},
		},
		{
			name:      "uppercase rpm names are legitimate",
			ids:       []string{"NetworkManager.x86_64=1:1.46.0-4.el9"},
			wantSpecs: []string{"NetworkManager-1:1.46.0-4.el9.x86_64"},
		},
		{
			name:      "a dot in the package name is not an arch",
			ids:       []string{"python3.11-libs.x86_64=3.11.5-1.el9"},
			wantSpecs: []string{"python3.11-libs-3.11.5-1.el9.x86_64"},
		},
		{
			name:     "a dnf option smuggled through the id is refused",
			ids:      []string{"--setopt.x86_64=install_weak_deps=True"},
			wantFail: []string{"--setopt.x86_64=install_weak_deps=True"},
		},
		{
			name:     "a package dnf is not offering is refused",
			ids:      []string{"httpd.x86_64=0:2.4.57-5.el9"},
			wantFail: []string{"httpd.x86_64=0:2.4.57-5.el9"},
		},
		{
			name:     "an id with no evr is refused",
			ids:      []string{"bash.x86_64"},
			wantFail: []string{"bash.x86_64"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := newInstallResult()
			specs, idBySpec := dnfInstallPlan(tt.ids, offered, &res)
			assertStrings(t, "specs", specs, tt.wantSpecs)
			assertFailed(t, res, tt.wantFail)
			if len(idBySpec) != len(specs) {
				t.Errorf("specs %v and idBySpec %v disagree", specs, idBySpec)
			}
		})
	}
}

func TestOfferedIDSets(t *testing.T) {
	updates := []Update{{ID: "openssl=3.3.1-1"}, {ID: "linux=6.9.2-1"}, {ID: "bare"}}
	ids := offeredIDs(updates)
	if !ids["openssl=3.3.1-1"] || !ids["linux=6.9.2-1"] || len(ids) != 3 {
		t.Errorf("offeredIDs = %v", ids)
	}
	names := pacmanOfferedNames(updates)
	if !names["openssl"] || !names["linux"] || names["bare"] || len(names) != 2 {
		t.Errorf("pacmanOfferedNames = %v", names)
	}
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

func assertFailed(t *testing.T, res InstallResult, want []string) {
	t.Helper()
	if len(res.Failed) != len(want) {
		t.Fatalf("Failed = %v, want exactly %v", res.Failed, want)
	}
	for _, id := range want {
		if _, ok := res.Failed[id]; !ok {
			t.Errorf("%q was dropped without a reason; the operator would never learn why", id)
		}
	}
}
