// Package heartbeat publishes the liveness beacon on plain core NATS every
// 30 s ± 3 s of jitter. Never spooled; the server marks agents offline at 90 s.
package heartbeat

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/openrmm/agent/internal/wire"
)

const (
	interval = 30 * time.Second
	jitter   = 3 * time.Second
)

type beat struct {
	Version         string `json:"version"`
	UptimeS         int64  `json:"uptime_s"`
	ScheduleVersion int    `json:"schedule_version"`
	Seq             uint64 `json:"seq"`
}

// Run publishes heartbeats until ctx is cancelled. The first beat goes out
// immediately so the server sees the agent online without a 30 s wait.
func Run(ctx context.Context, nc *nats.Conn, agentID, version string, log *slog.Logger) error {
	start := time.Now()
	var seq uint64
	for {
		seq++
		if err := publish(nc, agentID, version, start, seq); err != nil {
			log.Warn("heartbeat publish failed", "seq", seq, "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nextInterval()):
		}
	}
}

func publish(nc *nats.Conn, agentID, version string, start time.Time, seq uint64) error {
	env, err := wire.NewEnvelope("heartbeat", agentID, wire.NewMsgID(), beat{
		Version: version,
		UptimeS: int64(time.Since(start).Seconds()),
		Seq:     seq,
	})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return nc.Publish(wire.Heartbeat(agentID), raw)
}

// nextInterval returns 30 s ± up to 3 s, re-rolled each beat so a fleet
// enrolled at the same moment drifts apart instead of thundering together.
func nextInterval() time.Duration {
	return interval - jitter + rand.N(2*jitter)
}
