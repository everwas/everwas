package inventory

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseTabPackagesDpkg(t *testing.T) {
	out := "adduser\t3.137ubuntu1\n" +
		"apt\t2.7.14build2\n" +
		"base-files\t13ubuntu10.1\n" +
		"\n" + // trailing blank line
		"noversionline\n" // malformed: no tab
	pkgs := parseTabPackages(out)
	want := []pkg{
		{"adduser", "3.137ubuntu1"},
		{"apt", "2.7.14build2"},
		{"base-files", "13ubuntu10.1"},
	}
	assertPackages(t, pkgs, want)
}

func TestParseTabPackagesRPM(t *testing.T) {
	out := "bash\t5.2.26-3.el9\n" +
		"coreutils\t9.4-6.el9\n" +
		"glibc\t2.34-100.el9_4\n"
	pkgs := parseTabPackages(out)
	want := []pkg{
		{"bash", "5.2.26-3.el9"},
		{"coreutils", "9.4-6.el9"},
		{"glibc", "2.34-100.el9_4"},
	}
	assertPackages(t, pkgs, want)
}

func TestParsePacmanPackages(t *testing.T) {
	out := "bash 5.2.026-2\n" +
		"coreutils 9.5-1\n" +
		"linux 6.9.7.arch1-1\n"
	pkgs := parsePacmanPackages(out)
	want := []pkg{
		{"bash", "5.2.026-2"},
		{"coreutils", "9.5-1"},
		{"linux", "6.9.7.arch1-1"},
	}
	assertPackages(t, pkgs, want)
}

func TestParseTabPackagesCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxPackages+500; i++ {
		fmt.Fprintf(&b, "pkg%d\t1.0-%d\n", i, i)
	}
	if got := len(parseTabPackages(b.String())); got != maxPackages {
		t.Errorf("cap: got %d packages, want %d", got, maxPackages)
	}
}

func TestParsePackagesEmpty(t *testing.T) {
	if got := parseTabPackages(""); got == nil || len(got) != 0 {
		t.Errorf("empty input: got %#v, want empty non-nil slice", got)
	}
	if got := parsePacmanPackages(""); got == nil || len(got) != 0 {
		t.Errorf("empty input: got %#v, want empty non-nil slice", got)
	}
}

func assertPackages(t *testing.T, got, want []pkg) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d packages, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("package %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
