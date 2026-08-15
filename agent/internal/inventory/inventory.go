// Package inventory publishes full snapshots (hardware, software, processes,
// services) at startup and every 30 minutes on the INVENTORY JetStream
// subjects. Each snapshot carries a snapshot_hash — the sha256 of the
// canonical JSON of the payload — so the server can skip unchanged data.
package inventory

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/openrmm/agent/internal/wire"
)

const interval = 30 * time.Minute

type collector struct {
	kind    string
	collect func(context.Context) (any, error)
}

// Run publishes all snapshot kinds immediately, then every 30 minutes, until
// ctx is cancelled.
func Run(ctx context.Context, nc *nats.Conn, agentID string, log *slog.Logger) error {
	collectors := []collector{
		{"hardware", collectHardware},
		{"software", collectSoftware},
		{"processes", collectProcesses},
		{"services", collectServices},
	}
	for {
		for _, c := range collectors {
			payload, err := c.collect(ctx)
			if err != nil {
				log.Warn("inventory collect failed", "kind", c.kind, "err", err)
				continue
			}
			if err := publishSnapshot(nc, agentID, c.kind, payload); err != nil {
				log.Warn("inventory publish failed", "kind", c.kind, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

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
