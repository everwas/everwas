package inventory

import (
	"context"
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
	return collectSoftwareWith(ctx, run)
}

// collectSoftwareWith enumerates installed packages.
//
// Returns an error rather than an empty list when the enumeration fails, for
// the reason spelled out in services.go: the server treats a snapshot as
// complete, so an empty list retires every package on the host.
func collectSoftwareWith(ctx context.Context, exec runner) (any, error) {
	pkgs, err := listPackages(ctx, exec)
	if err != nil {
		return nil, err
	}
	return softwareSnapshot{Packages: pkgs}, nil
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
