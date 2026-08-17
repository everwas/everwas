// Package telemetry samples host metrics every 60 s and publishes them on the
// TELEMETRY JetStream subject. Every field is best-effort: a metric that
// fails to collect is logged and zeroed, never fatal.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"

	"github.com/rsp2k/openrmm/agent/internal/wire"
)

const interval = 60 * time.Second

type sample struct {
	CPUPct   float64 `json:"cpu_pct"`
	MemUsed  uint64  `json:"mem_used"`
	MemTotal uint64  `json:"mem_total"`
	SwapPct  float64 `json:"swap_pct"`
	// Load1 is a POINTER so the field can be absent rather than zero.
	//
	// Windows has no load average. gopsutil still answers there, from a
	// simulated background sampler, and what it answers is meaningless: a real
	// Windows host reported 7.5e-50, which is a denormal that Postgres cannot
	// store in a real column at all, so the server rejected the entire
	// telemetry sample (cpu, memory, disks, network, alert evaluation) once a
	// minute for hours. Reporting 0.0 instead would have been storable and
	// worse: a flat zero line on a dashboard is a claim that the machine is
	// idle, not an admission that the metric does not exist here.
	Load1   *float64     `json:"load1,omitempty"`
	UptimeS uint64       `json:"uptime_s"`
	Disks   []diskUsage  `json:"disks"`
	Nets    []netCounter `json:"nets,omitempty"`
}

// netCounter is one interface's CUMULATIVE counters since boot, not a rate.
//
// Rates are computed server-side from consecutive samples, for two reasons.
// Sending a rate would bake in this agent's sampling interval, so changing
// the interval would silently rescale every historical value; and a counter
// survives a missed sample, where a rate does not -- if one publish is lost,
// the next delta still covers the whole gap.
//
// The consumer MUST handle counters going backwards. They reset on reboot,
// on driver reload, and on some virtual NICs at 32-bit wraparound, and a
// naive subtraction then reports a spike of several exabytes per second.
type netCounter struct {
	Name        string `json:"name"`
	BytesSent   uint64 `json:"bytes_sent"`
	BytesRecv   uint64 `json:"bytes_recv"`
	PacketsSent uint64 `json:"packets_sent"`
	PacketsRecv uint64 `json:"packets_recv"`
	ErrIn       uint64 `json:"err_in"`
	ErrOut      uint64 `json:"err_out"`
	DropIn      uint64 `json:"drop_in"`
	DropOut     uint64 `json:"drop_out"`
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
	if runtime.GOOS != "windows" {
		if avg, err := load.AvgWithContext(ctx); err == nil {
			s.Load1 = &avg.Load1
		} else {
			log.Warn("collect load average", "err", err)
		}
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.UptimeS = up
	} else {
		log.Warn("collect uptime", "err", err)
	}
	s.Disks = collectDisks(ctx, log)
	s.Nets = collectNets(ctx, log)
	return s
}

// collectNets reads per-interface counters.
//
// Loopback is skipped: it carries real traffic on any busy host and none of
// it says anything about the network, so leaving it in makes the busiest row
// in the table the one nobody cares about.
func collectNets(ctx context.Context, log *slog.Logger) []netCounter {
	stats, err := psnet.IOCountersWithContext(ctx, true)
	if err != nil {
		log.Warn("net counters", "err", err)
		return nil
	}
	out := make([]netCounter, 0, len(stats))
	for _, st := range stats {
		name := strings.ToLower(st.Name)
		if name == "lo" || name == "lo0" || strings.HasPrefix(name, "loopback") {
			continue
		}
		// An interface that has never moved a byte is noise on a machine with
		// a dozen virtual adapters, and it is the common case on Windows.
		if st.BytesSent == 0 && st.BytesRecv == 0 {
			continue
		}
		out = append(out, netCounter{
			Name:        st.Name,
			BytesSent:   st.BytesSent,
			BytesRecv:   st.BytesRecv,
			PacketsSent: st.PacketsSent,
			PacketsRecv: st.PacketsRecv,
			ErrIn:       st.Errin,
			ErrOut:      st.Errout,
			DropIn:      st.Dropin,
			DropOut:     st.Dropout,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
