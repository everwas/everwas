//go:build darwin

package inventory

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// collectDMI reads identity from system_profiler, the same tool sysinfo
// already shells out to for the OS version. Macs have no DMI tables; the
// hardware overview is the equivalent. Manufacturer is constant — Apple has
// never OEM'd this field — and chassis is inferred from the marketing name
// because Apple Silicon model identifiers ("Mac15,7") no longer encode it.
func collectDMI(ctx context.Context) dmiInfo {
	out, err := exec.CommandContext(ctx, "system_profiler", "SPHardwareDataType", "-json").Output()
	if err != nil {
		return dmiInfo{}
	}
	return parseHardwareOverview(out)
}

func parseHardwareOverview(raw []byte) dmiInfo {
	var doc struct {
		SPHardwareDataType []struct {
			MachineModel string `json:"machine_model"`
			MachineName  string `json:"machine_name"`
			SerialNumber string `json:"serial_number"`
		} `json:"SPHardwareDataType"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.SPHardwareDataType) == 0 {
		return dmiInfo{}
	}
	hw := doc.SPHardwareDataType[0]

	return dmiInfo{
		Manufacturer: "Apple Inc.",
		Model:        cleanDMI(hw.MachineModel),
		Serial:       cleanDMI(hw.SerialNumber),
		Chassis:      appleChassis(hw.MachineName),
	}
}

func appleChassis(machineName string) string {
	name := strings.ToLower(machineName)
	switch {
	case strings.HasPrefix(name, "macbook"):
		return "laptop"
	case strings.HasPrefix(name, "imac"):
		return "all-in-one"
	case strings.HasPrefix(name, "mac mini"), strings.HasPrefix(name, "macmini"),
		strings.HasPrefix(name, "mac studio"), strings.HasPrefix(name, "mac pro"):
		return "desktop"
	default:
		return ""
	}
}
