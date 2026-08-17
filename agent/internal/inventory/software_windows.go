package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// uninstallKey is one place Windows records installed software.
//
// Three are needed, not one. A 64-bit process reading
// SOFTWARE\Microsoft\...\Uninstall sees only 64-bit products: the WOW6432Node
// path holds 32-bit ones, and plenty of real software is still 32-bit. HKCU
// holds per-user installs, which is where a surprising amount of modern
// software lives (anything installed without elevation).
type uninstallKey struct {
	root registry.Key
	path string
	// access is the extra flag needed to see the intended registry view. The
	// WOW64 redirector is otherwise invisible and silently returns the wrong
	// half of the machine's software.
	access uint32
}

var uninstallKeys = []uninstallKey{
	{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, registry.WOW64_64KEY},
	{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, registry.WOW64_32KEY},
	// The agent runs as LocalSystem, so this is SYSTEM's own hive and will
	// usually be empty. It is read anyway because the same binary run
	// interactively during troubleshooting should report what the operator
	// sees, and an empty key costs one failed open.
	{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, 0},
}

// listPackages enumerates installed software from the registry.
//
// Deliberately NOT Win32_Product. That WMI class is the obvious answer and the
// wrong one: enumerating it makes the Installer service reconfigure every MSI
// product on the machine, which is slow, writes to the event log, and has been
// known to reinstall or repair software as a side effect of being *asked what
// is installed*. An inventory agent must never change the machine it is
// inventorying. The registry is what Add/Remove Programs itself reads.
func listPackages(ctx context.Context, _ runner) ([]pkg, error) {
	seen := make(map[pkg]struct{})
	for _, k := range uninstallKeys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := readUninstallKey(ctx, k)
		if err != nil {
			// Enumeration failed on a key that exists. Reporting the packages
			// we happened to read before the failure would understate what is
			// installed, and the server records anything missing from a
			// snapshot as removed.
			return nil, fmt.Errorf("software: %s: %w", k.path, err)
		}
		for _, p := range found {
			seen[p] = struct{}{}
		}
	}

	pkgs := make([]pkg, 0, len(seen))
	for p := range seen {
		pkgs = append(pkgs, p)
	}
	// Sorting is required, not cosmetic. The snapshot is hashed to decide
	// whether anything changed, map iteration order is randomised, and registry
	// enumeration order is not promised either, so unsorted output would report
	// a "change" on every single poll.
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].Name != pkgs[j].Name {
			return pkgs[i].Name < pkgs[j].Name
		}
		return pkgs[i].Version < pkgs[j].Version
	})
	if len(pkgs) > maxPackages {
		pkgs = pkgs[:maxPackages]
	}
	return pkgs, nil
}

func readUninstallKey(ctx context.Context, k uninstallKey) ([]pkg, error) {
	key, err := registry.OpenKey(k.root, k.path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE|k.access)
	if err != nil {
		// A missing key is genuinely normal and is NOT a failure to look:
		// WOW6432Node does not exist on 32-bit Windows, and SYSTEM's HKCU may
		// have no Uninstall key at all. Anything else is a real failure, and
		// access-denied in particular must not read as "nothing installed".
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer key.Close()

	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		// The key opened and then would not enumerate. Never normal.
		return nil, err
	}

	pkgs := make([]pkg, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			break
		}
		if p, ok := readUninstallEntry(key, name, k.access); ok {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

// readUninstallEntry reads one product, reporting whether it counts as
// installed software a human would recognise.
func readUninstallEntry(parent registry.Key, name string, access uint32) (pkg, bool) {
	sub, err := registry.OpenKey(parent, name, registry.QUERY_VALUE|access)
	if err != nil {
		return pkg{}, false
	}
	defer sub.Close()

	display, _, err := sub.GetStringValue("DisplayName")
	display = strings.TrimSpace(display)
	if err != nil || display == "" {
		// No DisplayName means Add/Remove Programs would not show it either.
		// These are mostly install-time leftovers and GUID-named fragments.
		return pkg{}, false
	}

	// SystemComponent marks things Windows hides from Add/Remove Programs:
	// runtime bits, driver packages, MSI shims. Including them buries the
	// software an operator actually cares about in hundreds of rows.
	if n, _, err := sub.GetIntegerValue("SystemComponent"); err == nil && n == 1 {
		return pkg{}, false
	}

	// ParentKeyName / ParentDisplayName mark an entry as an UPDATE to another
	// product rather than a product. Patch state is collected separately from
	// the Windows Update agent, and listing hotfixes here would both duplicate
	// that and swamp the package list.
	if v, _, err := sub.GetStringValue("ParentKeyName"); err == nil && strings.TrimSpace(v) != "" {
		return pkg{}, false
	}
	if v, _, err := sub.GetStringValue("ReleaseType"); err == nil {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "security update", "update rollup", "hotfix", "update":
			return pkg{}, false
		}
	}

	version, _, err := sub.GetStringValue("DisplayVersion")
	if err != nil {
		version = ""
	}
	return pkg{Name: display, Version: strings.TrimSpace(version)}, true
}
