//go:build linux

package inventory

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// dmiSysfsDir is where the kernel exposes SMBIOS strings. Absent on
// machines without DMI tables (ARM SBCs, some VMs), which is not an error:
// the machine genuinely has nothing to say.
const dmiSysfsDir = "/sys/class/dmi/id"

func collectDMI(_ context.Context) dmiInfo {
	return readDMI(dmiSysfsDir)
}

// readDMI reads identity from a sysfs-shaped directory. The directory is a
// parameter so tests can point it at a fixture; production always passes
// dmiSysfsDir. product_serial is root-only (0400) — the agent runs as root
// as a service, but an unprivileged run just loses the serial, not the
// snapshot.
func readDMI(dir string) dmiInfo {
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return cleanDMI(string(raw))
	}

	info := dmiInfo{
		Manufacturer: read("sys_vendor"),
		Model:        read("product_name"),
		Serial:       read("product_serial"),
	}
	if raw := read("chassis_type"); raw != "" {
		if code, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			info.Chassis = chassisName(code)
		}
	}
	return info
}
