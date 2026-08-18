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

	"github.com/everwas/everwas/agent/internal/wire"
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

	// The 802.1X certificate this device is ACTUALLY holding, which is not the
	// same fact as the one the server last issued. They diverge on a renewal
	// that half-failed, on a machine restored from a backup image or cloned
	// from a template, and on material deleted by hand. Every one of those is
	// otherwise invisible until it surfaces as an authentication failure
	// nobody can account for.
	//
	// Omitted entirely when the device holds nothing, so a fleet that does not
	// use 802.1X does not pay for two null fields on every beat from every
	// machine every thirty seconds.
	CertSerial   string `json:"cert_serial,omitempty"`
	CertNotAfter string `json:"cert_not_after,omitempty"`
}

// CertFunc reports the serial and expiry of the certificate on disk. Empty
// when the device holds none.
type CertFunc func() (serial string, notAfter time.Time)

// ScheduleVersionFunc reports the schedule version the agent currently has
// cached, so the server can tell which agents are running a stale schedule.
type ScheduleVersionFunc func() int

// Run publishes heartbeats until ctx is cancelled. The first beat goes out
// immediately so the server sees the agent online without a 30 s wait.
func Run(
	ctx context.Context,
	nc *nats.Conn,
	agentID, version string,
	schedVersion ScheduleVersionFunc,
	cert CertFunc,
	log *slog.Logger,
) error {
	if schedVersion == nil {
		schedVersion = func() int { return 0 }
	}
	if cert == nil {
		cert = func() (string, time.Time) { return "", time.Time{} }
	}
	start := time.Now()
	var seq uint64
	for {
		seq++
		if err := publish(nc, agentID, version, start, seq, schedVersion(), cert); err != nil {
			log.Warn("heartbeat publish failed", "seq", seq, "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nextInterval()):
		}
	}
}

func publish(
	nc *nats.Conn,
	agentID, version string,
	start time.Time,
	seq uint64,
	schedVersion int,
	cert CertFunc,
) error {
	b := beat{
		Version:         version,
		UptimeS:         int64(time.Since(start).Seconds()),
		ScheduleVersion: schedVersion,
		Seq:             seq,
	}
	if serial, notAfter := cert(); serial != "" {
		b.CertSerial = serial
		b.CertNotAfter = notAfter.UTC().Format(time.RFC3339)
	}
	env, err := wire.NewEnvelope("heartbeat", agentID, wire.NewMsgID(), b)
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
