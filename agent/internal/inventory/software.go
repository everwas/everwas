package inventory

import (
	"context"
	"runtime"
	"strings"
)

// maxPackages caps the software snapshot; beyond this the tail is dropped.
const maxPackages = 5000

type pkg struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type softwareSnapshot struct {
	Packages []pkg `json:"packages"`
}

func collectSoftware(ctx context.Context) (any, error) {
	return softwareSnapshot{Packages: listPackages(ctx)}, nil
}

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

// parseTabPackages parses "name<TAB>version" lines (dpkg-query and rpm with
// the query formats above).
func parseTabPackages(out string) []pkg {
	pkgs := []pkg{}
	for line := range strings.Lines(out) {
		name, version, ok := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		if !ok || name == "" {
			continue
		}
		pkgs = append(pkgs, pkg{Name: name, Version: version})
		if len(pkgs) == maxPackages {
			break
		}
	}
	return pkgs
}

// parsePacmanPackages parses "name version" lines from `pacman -Q`.
func parsePacmanPackages(out string) []pkg {
	pkgs := []pkg{}
	for line := range strings.Lines(out) {
		name, version, ok := strings.Cut(strings.TrimRight(line, "\n"), " ")
		if !ok || name == "" {
			continue
		}
		pkgs = append(pkgs, pkg{Name: name, Version: version})
		if len(pkgs) == maxPackages {
			break
		}
	}
	return pkgs
}
