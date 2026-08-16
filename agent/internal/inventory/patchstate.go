package inventory

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/patch"
)

const (
	// KindPatchState is the inventory kind published on
	// agents.{id}.inventory.patchstate.
	KindPatchState = "patchstate"

	// patchInterval is the base scan cadence. A patch scan is expensive
	// (apt-get update, a Windows Update search that can run ten minutes),
	// so it runs far less often than the 30 minute inventory cycle.
	patchInterval = 6 * time.Hour

	// patchJitter spreads the fleet's scans out. It is derived from the
	// agent id rather than drawn at random, so a host's offset is stable
	// across restarts and two agents on the same box do not converge.
	patchJitter = 30 * time.Minute

	// backendUnsupported is reported when the host has no update backend,
	// which is more useful to the console than publishing nothing at all.
	backendUnsupported = "unsupported"
)

// PatchEntry is one available update as published in the snapshot.
type PatchEntry struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Kind         string   `json:"kind"`
	Severity     string   `json:"severity"`
	SizeBytes    int64    `json:"size_bytes"`
	RebootLikely bool     `json:"reboot_likely"`
	Unsupported  bool     `json:"unsupported"`
	Detail       string   `json:"detail,omitempty"`
	KBIDs        []string `json:"kb_ids,omitempty"`
}

// PatchState is the patchstate snapshot payload. publishSnapshot folds
// snapshot_hash in on the way out.
type PatchState struct {
	Patches        []PatchEntry `json:"patches"`
	Backend        string       `json:"backend"`
	RebootRequired bool         `json:"reboot_required"`
}

// PatchCollector owns the OS update backend and publishes patchstate.
//
// It exists as a struct rather than a package function because the backend
// must be detected exactly once: on Windows a Manager owns a locked OS
// thread with an initialised COM apartment, and re-detecting per scan would
// leak one thread per cycle.
type PatchCollector struct {
	nc      *nats.Conn
	agentID string
	log     *slog.Logger

	mu       sync.Mutex
	mgr      patch.Manager
	detected bool
	detErr   error
}

// NewPatchCollector builds a collector bound to a connection and identity.
func NewPatchCollector(nc *nats.Conn, agentID string, log *slog.Logger) *PatchCollector {
	return &PatchCollector{nc: nc, agentID: agentID, log: log}
}

// Manager returns the detected backend, detecting on first use. The error
// is cached too: a host with no package manager will not grow one.
func (c *PatchCollector) Manager() (patch.Manager, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.detected {
		c.detected = true
		mgr, err := patch.Detect()
		if err != nil {
			c.detErr = err
		} else {
			c.mgr = patch.WithLogger(mgr, c.log)
		}
	}
	return c.mgr, c.detErr
}

// Run publishes a patchstate snapshot immediately, then every 6 hours plus
// a per-agent offset, until ctx is cancelled.
func (c *PatchCollector) Run(ctx context.Context) error {
	interval := patchInterval + jitterFor(c.agentID, patchJitter)
	for {
		if _, err := c.RefreshNow(ctx); err != nil {
			c.log.Warn("patch scan failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// RefreshNow runs one scan and publishes the snapshot. It is the patch.scan
// job handler's collector, so an operator can force a scan without waiting
// out the six hour cycle.
//
// A host with no supported backend still publishes: an empty patch list
// with backend "unsupported" tells the console the truth, where silence
// would look like an agent that has stopped reporting.
func (c *PatchCollector) RefreshNow(ctx context.Context) (PatchState, error) {
	state, err := c.collect(ctx)
	if perr := c.publish(state); perr != nil {
		c.log.Warn("patchstate publish failed", "err", perr)
		if err == nil {
			err = perr
		}
	}
	return state, err
}

// collect runs the scan. It always returns a publishable state, even on
// error, so a failed scan still reports which backend was tried.
func (c *PatchCollector) collect(ctx context.Context) (PatchState, error) {
	state := PatchState{Patches: []PatchEntry{}, Backend: backendUnsupported}

	mgr, err := c.Manager()
	if err != nil {
		if errors.Is(err, patch.ErrUnsupported) {
			c.log.Info("no patch backend on this host, publishing an empty patchstate")
			return state, nil
		}
		return state, err
	}
	state.Backend = mgr.Kind()

	updates, err := mgr.Scan(ctx)
	if err != nil {
		return state, err
	}
	state.Patches = patchEntries(updates)

	// A reboot flag that cannot be read is not a scan failure: the update
	// list is still worth publishing.
	rebootRequired, rerr := mgr.RebootRequired(ctx)
	if rerr != nil {
		c.log.Debug("reboot-required check unavailable", "backend", state.Backend, "err", rerr)
	}
	state.RebootRequired = rebootRequired
	return state, nil
}

// publish sends the snapshot on the INVENTORY stream.
func (c *PatchCollector) publish(state PatchState) error {
	if c.nc == nil {
		return nil
	}
	return publishSnapshot(c.nc, c.agentID, KindPatchState, state)
}

// patchEntries converts backend updates into the wire shape.
func patchEntries(updates []patch.Update) []PatchEntry {
	entries := make([]PatchEntry, 0, len(updates))
	for _, u := range updates {
		entries = append(entries, PatchEntry{
			ID:           u.ID,
			Title:        u.Title,
			Kind:         u.Kind,
			Severity:     u.Severity,
			SizeBytes:    u.SizeBytes,
			RebootLikely: u.RebootLikely,
			Unsupported:  u.Unsupported,
			Detail:       u.Detail,
			KBIDs:        u.KBIDs,
		})
	}
	return entries
}

// jitterFor derives a stable per-agent offset in [0, window). Deriving it
// from the id instead of drawing it at random means a restarting agent does
// not re-roll into the same minute as everyone else.
func jitterFor(agentID string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(agentID))
	return time.Duration(h.Sum64() % uint64(window))
}
