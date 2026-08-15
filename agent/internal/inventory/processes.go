package inventory

import (
	"context"
	"sort"

	"github.com/shirou/gopsutil/v4/process"
)

// maxProcesses caps the process snapshot at the top N by resident memory.
const maxProcesses = 500

type proc struct {
	PID    int32   `json:"pid"`
	Name   string  `json:"name"`
	CPUPct float64 `json:"cpu_pct"`
	MemRSS uint64  `json:"mem_rss"`
}

func collectProcesses(ctx context.Context) (any, error) {
	ps, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	procs := make([]proc, 0, len(ps))
	for _, p := range ps {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue // exited between listing and stat
		}
		entry := proc{PID: p.Pid, Name: name}
		if mi, err := p.MemoryInfoWithContext(ctx); err == nil && mi != nil {
			entry.MemRSS = mi.RSS
		}
		// Single call: lifetime-average CPU, not an interval sample. Good
		// enough for a snapshot and avoids 500 sleeps.
		if pct, err := p.CPUPercentWithContext(ctx); err == nil {
			entry.CPUPct = pct
		}
		procs = append(procs, entry)
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].MemRSS > procs[j].MemRSS })
	if len(procs) > maxProcesses {
		procs = procs[:maxProcesses]
	}
	return processesSnapshot{Processes: procs}, nil
}

type processesSnapshot struct {
	Processes []proc `json:"processes"`
}
