//go:build !windows

package inventory

import (
	"context"
	"runtime"
)

// listPackages asks whichever package manager is present, in dpkg → rpm →
// pacman order. A manager that exists but reports nothing (e.g. rpm installed
// as a packaging tool on an Arch host) falls through to the next. Non-Linux
// platforms report an empty list for now (M2).
func listPackages(ctx context.Context) []pkg {
	if runtime.GOOS != "linux" {
		return []pkg{}
	}
	queries := []func() []pkg{
		func() []pkg {
			return parseTabPackages(run(ctx, "dpkg-query", "-W", "-f", "${Package}\t${Version}\n"))
		},
		func() []pkg {
			return parseTabPackages(run(ctx, "rpm", "-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\n"))
		},
		func() []pkg {
			return parsePacmanPackages(run(ctx, "pacman", "-Q"))
		},
	}
	for i, name := range []string{"dpkg-query", "rpm", "pacman"} {
		if !have(name) {
			continue
		}
		if pkgs := queries[i](); len(pkgs) > 0 {
			return pkgs
		}
	}
	return []pkg{}
}
