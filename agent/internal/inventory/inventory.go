// Package inventory publishes full snapshots (hardware, software, processes,
// services) at startup and every 30 minutes on the INVENTORY JetStream
// subjects. Each snapshot carries a snapshot_hash — the sha256 of the
// canonical JSON of the payload — so the server can skip unchanged data.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/wire"
)

const interval = 30 * time.Minute

type collector struct {
	kind    string
	collect func(context.Context) (any, error)
}

// Run publishes all snapshot kinds immediately, then every 30 minutes, until
// ctx is cancelled.
func Run(ctx context.Context, nc *nats.Conn, agentID string, log *slog.Logger) error {
	for {
		if err := RefreshNow(ctx, nc, agentID, log); err != nil {
			log.Warn("inventory refresh incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// RefreshNow publishes one full round of snapshots. It is also the
// inventory.refresh job handler, so an operator can force a snapshot
// without waiting out the 30 minute cycle. A collector that fails is logged
// and skipped; the error it returns names the kinds that did not publish.
func RefreshNow(ctx context.Context, nc *nats.Conn, agentID string, log *slog.Logger) error {
	collectors := []collector{
		{"hardware", collectHardware},
		{"software", collectSoftware},
		{"network", collectNetwork},
		{"logins", collectLogins},
		{"posture", collectPosture},
		{"processes", collectProcesses},
		{"services", collectServices},
	}
	var failed []string
	for _, c := range collectors {
		payload, err := c.collect(ctx)
		if err != nil {
			if !kindFailed(err) {
				// A platform gap, not a fault. Logged once per cycle at info
				// so it is discoverable, but it must not publish and must not
				// count as a failure: nobody can fix "macOS has no software
				// collector yet" by looking at a warning every 30 minutes.
				log.Info("inventory kind not collected here", "kind", c.kind, "reason", err)
				continue
			}
			log.Warn("inventory collect failed", "kind", c.kind, "err", err)
			failed = append(failed, c.kind)
			continue
		}
		if err := publishSnapshot(nc, agentID, c.kind, payload); err != nil {
			log.Warn("inventory publish failed", "kind", c.kind, "err", err)
			failed = append(failed, c.kind)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("inventory kinds failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// kindFailed reports whether an error from a collector is a real failure to
// look, as opposed to this platform simply not having that collector.
//
// The distinction matters because both must skip the publish (an empty
// snapshot is a false claim either way) but only one is worth an operator's
// attention.
func kindFailed(err error) bool { return !errors.Is(err, errNoCollector) }

// publishSnapshot hashes the payload, folds snapshot_hash into the data
// object, and publishes with a Nats-Msg-Id header for JetStream dedup.
func publishSnapshot(nc *nats.Conn, agentID, kind string, payload any) error {
	data, err := withSnapshotHash(payload)
	if err != nil {
		return err
	}
	env, err := wire.NewEnvelope("inventory", agentID, wire.NewMsgID(), data)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return nc.PublishMsg(&nats.Msg{
		Subject: wire.Inventory(agentID, kind),
		Data:    raw,
		Header:  nats.Header{"Nats-Msg-Id": []string{env.MsgID}},
	})
}

// withSnapshotHash returns the payload as a generic object with a
// snapshot_hash field added. The hash covers the payload WITHOUT the hash
// field, in canonical (sorted-key) JSON form.
func withSnapshotHash(payload any) (map[string]any, error) {
	hash, err := snapshotHash(payload)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	data["snapshot_hash"] = hash
	return data, nil
}
