package inventory

import "testing"

// snapshotHash must not depend on how the payload was represented in Go —
// struct vs map, field declaration order — only on the JSON content.
func TestSnapshotHashKeyOrderIndependent(t *testing.T) {
	type ab struct {
		A string `json:"a"`
		B int    `json:"b"`
	}
	type ba struct {
		B int    `json:"b"`
		A string `json:"a"`
	}
	h1, err := snapshotHash(ab{A: "x", B: 2})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := snapshotHash(ba{B: 2, A: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("field order changed hash: %s vs %s", h1, h2)
	}
	h3, err := snapshotHash(map[string]any{"b": 2, "a": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h3 {
		t.Errorf("struct vs map changed hash: %s vs %s", h1, h3)
	}
}

func TestSnapshotHashDetectsChange(t *testing.T) {
	h1, _ := snapshotHash(map[string]any{"a": 1})
	h2, _ := snapshotHash(map[string]any{"a": 2})
	if h1 == h2 {
		t.Error("different payloads hashed identically")
	}
}

func TestSnapshotHashLargeIntegersExact(t *testing.T) {
	// Byte counts overflow float64 precision above 2^53; UseNumber must keep
	// adjacent values distinct.
	h1, _ := snapshotHash(map[string]uint64{"n": 1 << 60})
	h2, _ := snapshotHash(map[string]uint64{"n": 1<<60 + 1})
	if h1 == h2 {
		t.Error("adjacent uint64 values hashed identically (float64 precision loss)")
	}
}

func TestSnapshotHashStable(t *testing.T) {
	payload := map[string]any{"packages": []pkg{{"bash", "5.2"}, {"vim", "9.1"}}}
	h1, _ := snapshotHash(payload)
	h2, _ := snapshotHash(payload)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length %d, want 64 hex chars", len(h1))
	}
}

func TestWithSnapshotHashEmbedsHash(t *testing.T) {
	payload := softwareSnapshot{Packages: []pkg{{"bash", "5.2"}}}
	want, err := snapshotHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := withSnapshotHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	if data["snapshot_hash"] != want {
		t.Errorf("snapshot_hash = %v, want %s", data["snapshot_hash"], want)
	}
	if _, ok := data["packages"]; !ok {
		t.Error("payload fields lost when embedding snapshot_hash")
	}
}
