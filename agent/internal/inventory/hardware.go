package inventory

import (
	"context"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/everwas/everwas/agent/internal/sysinfo"
)

type hardware struct {
	CPUModel       string `json:"cpu_model"`
	CPUCores       int    `json:"cpu_cores"`
	MemTotal       uint64 `json:"mem_total"`
	Hostname       string `json:"hostname"`
	OSFamily       string `json:"os_family"`
	OSVersion      string `json:"os_version"`
	Kernel         string `json:"kernel"`
	Arch           string `json:"arch"`
	Virtualization string `json:"virtualization"`

	// Machine identity from SMBIOS/DMI. omitempty is the contract: an agent
	// that cannot read these omits them, and the server records no identity
	// belief at all — absent means "cannot say", never "has no serial".
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	ChassisType  string `json:"chassis_type,omitempty"`
}

func collectHardware(ctx context.Context) (any, error) {
	info, err := host.InfoWithContext(ctx)
	if err != nil {
		return nil, err
	}
	hw := hardware{
		Hostname:       info.Hostname,
		OSFamily:       sysinfo.OSFamily(),
		OSVersion:      sysinfo.OSVersion(),
		Kernel:         info.KernelVersion,
		Arch:           sysinfo.Arch(),
		Virtualization: info.VirtualizationSystem,
	}
	if hw.OSVersion == "" {
		hw.OSVersion = strings.TrimSpace(info.Platform + " " + info.PlatformVersion)
	}
	if cpus, err := cpu.InfoWithContext(ctx); err == nil && len(cpus) > 0 {
		hw.CPUModel = cpus[0].ModelName
	}
	if cores, err := cpu.CountsWithContext(ctx, true); err == nil {
		hw.CPUCores = cores
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		hw.MemTotal = vm.Total
	}
	dmi := collectDMI(ctx)
	hw.Manufacturer = dmi.Manufacturer
	hw.Model = dmi.Model
	hw.SerialNumber = dmi.Serial
	hw.ChassisType = dmi.Chassis
	return hw, nil
}
