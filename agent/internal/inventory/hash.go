package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// snapshotHash returns the sha256 hex of the canonical JSON encoding of v.
// Canonical means object keys sorted, which encoding/json guarantees for
// maps — so we round-trip through an untyped decode first. UseNumber keeps
// large integers (byte counts) exact instead of drifting through float64.
func snapshotHash(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var untyped any
	if err := dec.Decode(&untyped); err != nil {
		return "", err
	}
	canon, err := json.Marshal(untyped)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}
