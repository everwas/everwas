package inventory

import (
	"context"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/rsp2k/openrmm/agent/internal/sysinfo"
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
	return hw, nil
}
