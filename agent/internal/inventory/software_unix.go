//go:build !windows

package inventory

import (
	"context"
	"fmt"
	"runtime"
)

// listPackages asks whichever package manager is present, in dpkg → rpm →
// pacman order. A manager that exists but reports nothing (e.g. rpm installed
// as a packaging tool on an Arch host) falls through to the next.
//
// A manager that FAILS does not fall through: it returns the error. Falling
// through on failure is how a dpkg-query timeout on a Debian host ends up
// reporting the empty result of asking pacman, which is not installed, as
// "this host has no software".
func listPackages(ctx context.Context, exec runner) ([]pkg, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("software: %w", errNoCollector)
	}
	queries := []struct {
		binary string
		list   func() ([]pkg, error)
	}{
		{"dpkg-query", func() ([]pkg, error) {
			out, err := exec(ctx, "dpkg-query", "-W", "-f", "${Package}\t${Version}\n")
			return parseTabPackages(out), err
		}},
		{"rpm", func() ([]pkg, error) {
			out, err := exec(ctx, "rpm", "-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\n")
			return parseTabPackages(out), err
		}},
		{"pacman", func() ([]pkg, error) {
			out, err := exec(ctx, "pacman", "-Q")
			return parsePacmanPackages(out), err
		}},
	}

	present := false
	for _, q := range queries {
		if !have(q.binary) {
			continue
		}
		present = true
		pkgs, err := q.list()
		if err != nil {
			return nil, fmt.Errorf("software: %s: %w", q.binary, err)
		}
		if len(pkgs) > 0 {
			return pkgs, nil
		}
	}
	if !present {
		// A Linux host with no package manager we recognise. Honest to say we
		// have no collector for it rather than to claim it has no software.
		return nil, fmt.Errorf("software: no supported package manager: %w", errNoCollector)
	}
	// Every present manager succeeded and reported nothing. Unusual, but it is
	// an answer rather than a failure.
	return []pkg{}, nil
}
