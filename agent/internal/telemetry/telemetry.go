// Package telemetry samples host metrics every 60 s and publishes them on the
// TELEMETRY JetStream subject. Every field is best-effort: a metric that
// fails to collect is logged and zeroed, never fatal.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/openrmm/agent/internal/wire"
)

const interval = 60 * time.Second

type sample struct {
	CPUPct   float64     `json:"cpu_pct"`
	MemUsed  uint64      `json:"mem_used"`
	MemTotal uint64      `json:"mem_total"`
	SwapPct  float64     `json:"swap_pct"`
	Load1    float64     `json:"load1"`
	UptimeS  uint64      `json:"uptime_s"`
	Disks    []diskUsage `json:"disks"`
}

type diskUsage struct {
	Mount  string `json:"mount"`
	Used   uint64 `json:"used"`
	Total  uint64 `json:"total"`
	FSType string `json:"fstype"`
}

// Run collects and publishes telemetry until ctx is cancelled.
func Run(ctx context.Context, nc *nats.Conn, agentID string, log *slog.Logger) error {
	for {
		publish(ctx, nc, agentID, log)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func publish(ctx context.Context, nc *nats.Conn, agentID string, log *slog.Logger) {
	env, err := wire.NewEnvelope("telemetry", agentID, wire.NewMsgID(), collect(ctx, log))
	if err != nil {
		log.Warn("telemetry envelope", "err", err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		log.Warn("telemetry marshal", "err", err)
		return
	}
	msg := &nats.Msg{
		Subject: wire.Telemetry(agentID),
		Data:    raw,
		Header:  nats.Header{"Nats-Msg-Id": []string{env.MsgID}},
	}
	if err := nc.PublishMsg(msg); err != nil {
		log.Warn("telemetry publish failed", "err", err)
	}
}

func collect(ctx context.Context, log *slog.Logger) sample {
	var s sample
	if pcts, err := cpu.PercentWithContext(ctx, time.Second, false); err == nil && len(pcts) > 0 {
		s.CPUPct = pcts[0]
	} else if err != nil {
		log.Warn("collect cpu", "err", err)
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.MemUsed, s.MemTotal = vm.Used, vm.Total
	} else {
		log.Warn("collect mem", "err", err)
	}
	if sm, err := mem.SwapMemoryWithContext(ctx); err == nil {
		s.SwapPct = sm.UsedPercent
	} else {
		log.Warn("collect swap", "err", err)
	}
	if avg, err := load.AvgWithContext(ctx); err == nil {
		s.Load1 = avg.Load1
	} // unix only; stays 0 elsewhere
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.UptimeS = up
	} else {
		log.Warn("collect uptime", "err", err)
	}
	s.Disks = collectDisks(ctx, log)
	return s
}

func collectDisks(ctx context.Context, log *slog.Logger) []diskUsage {
	disks := []diskUsage{}
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		log.Warn("collect disk partitions", "err", err)
		return disks
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if !realFS(p.Fstype) || seen[p.Mountpoint] {
			continue
		}
		seen[p.Mountpoint] = true
		u, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil {
			continue // transient mounts vanish between listing and stat
		}
		disks = append(disks, diskUsage{
			Mount:  p.Mountpoint,
			Used:   u.Used,
			Total:  u.Total,
			FSType: p.Fstype,
		})
	}
	return disks
}

// realFSTypes is the allowlist of filesystems worth reporting; everything
// else (proc, tmpfs, overlay, squashfs, ...) is pseudo or ephemeral.
var realFSTypes = map[string]bool{
	"ext4":  true,
	"xfs":   true,
	"btrfs": true,
	"zfs":   true,
	"apfs":  true,
	"hfs":   true,
	"ntfs":  true,
	"exfat": true,
	"vfat":  true,
	"fat":   true,
	"fat32": true, // Windows reports FAT volumes as FAT32/FAT16
	"fat16": true,
}

func realFS(fstype string) bool {
	return realFSTypes[strings.ToLower(fstype)]
}
