package inventory

import (
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/everwas/everwas/agent/internal/patch"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestPatchStateWireShape pins the published JSON down field by field. The
// server ingests this shape; a rename here is a protocol break.
func TestPatchStateWireShape(t *testing.T) {
	state := PatchState{
		Backend:        "apt",
		RebootRequired: true,
		Patches: []PatchEntry{{
			ID:           "libc6=2.31-13+deb11u5",
			Title:        "libc6 2.31-13+deb11u3 to 2.31-13+deb11u5",
			Kind:         "security",
			Severity:     "unknown",
			SizeBytes:    0,
			RebootLikely: true,
		}},
	}
	data, err := withSnapshotHash(state)
	if err != nil {
		t.Fatalf("withSnapshotHash: %v", err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"patches", "backend", "reboot_required", "snapshot_hash"} {
		if _, ok := got[key]; !ok {
			t.Errorf("published snapshot is missing %q: %s", key, raw)
		}
	}
	entry := got["patches"].([]any)[0].(map[string]any)
	for _, key := range []string{"id", "title", "kind", "severity", "size_bytes", "reboot_likely", "unsupported"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("patch entry is missing %q: %s", key, raw)
		}
	}
	// detail and kb_ids are omitted when empty rather than published as null.
	if _, ok := entry["detail"]; ok {
		t.Error("an empty detail must be omitted, not published")
	}
	if _, ok := entry["kb_ids"]; ok {
		t.Error("empty kb_ids must be omitted, not published")
	}
}

func TestPatchStateEmptyListMarshalsAsArray(t *testing.T) {
	raw, err := json.Marshal(PatchState{Patches: []PatchEntry{}, Backend: "apt"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); got != `{"patches":[],"backend":"apt","reboot_required":false}` {
		t.Errorf("empty snapshot = %s", got)
	}
}

func TestPatchEntries(t *testing.T) {
	got := patchEntries([]patch.Update{
		{
			ID: "7d5a4c1e", Title: "Cumulative Update", Kind: patch.KindSecurity,
			KBIDs: []string{"KB5034123"}, Severity: patch.SeverityCritical,
			SizeBytes: 100, RebootLikely: true,
		},
		{
			ID: "macOS Sequoia 15.5-24F74", Title: "macOS Sequoia 15.5",
			Kind: patch.KindFeature, Severity: patch.SeverityUnknown,
			Unsupported: true, Detail: "major macOS upgrades are not installed by the agent",
		},
	})
	want := []PatchEntry{
		{ID: "7d5a4c1e", Title: "Cumulative Update", Kind: "security", Severity: "critical",
			SizeBytes: 100, RebootLikely: true, KBIDs: []string{"KB5034123"}},
		{ID: "macOS Sequoia 15.5-24F74", Title: "macOS Sequoia 15.5", Kind: "feature",
			Severity: "unknown", Unsupported: true,
			Detail: "major macOS upgrades are not installed by the agent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
	if got := patchEntries(nil); len(got) != 0 || got == nil {
		t.Error("no updates must yield an empty slice, not nil")
	}
}

func TestPatchSnapshotHashIgnoresOrderOfEqualSnapshots(t *testing.T) {
	a := PatchState{Patches: []PatchEntry{{ID: "a"}}, Backend: "apt"}
	b := PatchState{Patches: []PatchEntry{{ID: "a"}}, Backend: "apt"}
	ha, err := snapshotHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := snapshotHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Error("identical snapshots must hash identically")
	}
	c := PatchState{Patches: []PatchEntry{{ID: "b"}}, Backend: "apt"}
	hc, _ := snapshotHash(c)
	if ha == hc {
		t.Error("different snapshots must hash differently")
	}
}

func TestJitterFor(t *testing.T) {
	window := 30 * time.Minute
	first := jitterFor("0198f6f2-0000-7000-8000-000000000001", window)
	if first < 0 || first >= window {
		t.Errorf("jitter %s is outside [0, %s)", first, window)
	}
	if again := jitterFor("0198f6f2-0000-7000-8000-000000000001", window); again != first {
		t.Error("jitter must be stable for the same agent id across restarts")
	}
	other := jitterFor("0198f6f2-0000-7000-8000-000000000002", window)
	if other == first {
		t.Error("two agents landing on the same offset defeats the point")
	}
	if got := jitterFor("anything", 0); got != 0 {
		t.Errorf("a zero window must yield no jitter, got %s", got)
	}
}

// TestPatchCollectorWithoutBackend proves a host with no package manager
// still publishes a snapshot rather than going quiet, which would look
// identical to an agent that had stopped reporting.
func TestPatchCollectorCollectDegradesWithoutBackend(t *testing.T) {
	c := NewPatchCollector(nil, "agent-1", testLogger())
	c.detected = true
	c.detErr = patch.ErrUnsupported

	state, err := c.RefreshNow(t.Context())
	if err != nil {
		t.Fatalf("an unsupported host must not be an error: %v", err)
	}
	if state.Backend != backendUnsupported {
		t.Errorf("backend = %q, want %q", state.Backend, backendUnsupported)
	}
	if len(state.Patches) != 0 {
		t.Errorf("patches = %+v, want none", state.Patches)
	}
}

func TestPatchCollectorManagerDetectsOnce(t *testing.T) {
	c := NewPatchCollector(nil, "agent-1", testLogger())
	first, firstErr := c.Manager()
	second, secondErr := c.Manager()
	if first != second {
		t.Error("Manager must return the same backend every call")
	}
	if (firstErr == nil) != (secondErr == nil) {
		t.Error("Manager must cache its detection outcome")
	}
}
