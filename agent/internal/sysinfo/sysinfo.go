// Package sysinfo answers small identity questions shared by enrollment and
// inventory: OS family, architecture, and OS version strings.
package sysinfo

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// OSFamily returns the wire value for this platform: linux, macos, windows.
func OSFamily() string { return osFamilyFor(runtime.GOOS) }

// Arch returns the wire value for this architecture: x86_64, aarch64, etc.
func Arch() string { return archFor(runtime.GOARCH) }

func osFamilyFor(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

func archFor(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

// OSVersion returns a human-readable OS version, best-effort. Empty string is
// acceptable on platforms with no cheap answer.
func OSVersion() string {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/etc/os-release")
		if err != nil {
			return ""
		}
		return prettyName(string(raw))
	case "darwin":
		out, err := exec.Command("sw_vers", "-productVersion").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	default:
		return ""
	}
}

// prettyName extracts PRETTY_NAME from os-release(5) content.
func prettyName(osRelease string) string {
	for _, line := range strings.Split(osRelease, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
