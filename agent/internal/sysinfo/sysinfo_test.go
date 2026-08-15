package sysinfo

import "testing"

func TestOSFamilyFor(t *testing.T) {
	cases := map[string]string{
		"linux":   "linux",
		"darwin":  "macos",
		"windows": "windows",
	}
	for goos, want := range cases {
		if got := osFamilyFor(goos); got != want {
			t.Errorf("osFamilyFor(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestArchFor(t *testing.T) {
	cases := map[string]string{
		"amd64":   "x86_64",
		"arm64":   "aarch64",
		"riscv64": "riscv64",
	}
	for goarch, want := range cases {
		if got := archFor(goarch); got != want {
			t.Errorf("archFor(%q) = %q, want %q", goarch, got, want)
		}
	}
}

func TestPrettyName(t *testing.T) {
	osRelease := `NAME="Ubuntu"
VERSION="24.04.1 LTS (Noble Numbat)"
ID=ubuntu
PRETTY_NAME="Ubuntu 24.04.1 LTS"
VERSION_ID="24.04"
`
	if got := prettyName(osRelease); got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("prettyName = %q, want %q", got, "Ubuntu 24.04.1 LTS")
	}
	if got := prettyName("ID=weird\n"); got != "" {
		t.Errorf("prettyName without PRETTY_NAME = %q, want empty", got)
	}
	// Arch Linux quotes nothing on some fields; unquoted values must survive.
	if got := prettyName("PRETTY_NAME=Arch Linux\n"); got != "Arch Linux" {
		t.Errorf("unquoted prettyName = %q, want %q", got, "Arch Linux")
	}
}
