//go:build windows

package inventory

import (
	"context"

	"github.com/yusufpapurcu/wmi"
)

// The three WMI classes that carry machine identity. Queried separately and
// best-effort: a hardened box that blocks Win32_SystemEnclosure still
// reports manufacturer and serial.

type win32ComputerSystem struct {
	Manufacturer string
	Model        string
}

type win32BIOS struct {
	SerialNumber string
}

type win32SystemEnclosure struct {
	ChassisTypes []int16
}

func collectDMI(_ context.Context) dmiInfo {
	var info dmiInfo

	var cs []win32ComputerSystem
	if err := wmi.Query("SELECT Manufacturer, Model FROM Win32_ComputerSystem", &cs); err == nil && len(cs) > 0 {
		info.Manufacturer = cleanDMI(cs[0].Manufacturer)
		info.Model = cleanDMI(cs[0].Model)
	}

	var bios []win32BIOS
	if err := wmi.Query("SELECT SerialNumber FROM Win32_BIOS", &bios); err == nil && len(bios) > 0 {
		info.Serial = cleanDMI(bios[0].SerialNumber)
	}

	var enc []win32SystemEnclosure
	if err := wmi.Query("SELECT ChassisTypes FROM Win32_SystemEnclosure", &enc); err == nil && len(enc) > 0 && len(enc[0].ChassisTypes) > 0 {
		// The field is an array but real machines report one entry; docks
		// and multi-enclosure cases are rare enough that the first entry is
		// the machine's own chassis.
		info.Chassis = chassisName(int(enc[0].ChassisTypes[0]))
	}

	return info
}
