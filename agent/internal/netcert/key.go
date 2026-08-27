package netcert

import (
	"crypto"
	"crypto/sha256"
	"fmt"
)

// deviceKey is the key a renewal will be certified for, together with what to
// do with it in each of the two outcomes.
//
// It exists because the two platforms disagree about WHEN the key becomes real.
// On Unix it is a few hundred bytes in memory until a certificate for it
// arrives, so a failed renewal leaves nothing anywhere. On Windows it is a
// container inside a CNG provider that has to exist before it can sign the CSR,
// so a failed renewal leaves something behind that has to be cleaned up. Hiding
// that difference behind one shape keeps Ensure a single piece of code with a
// single set of orderings to reason about.
type deviceKey struct {
	// signer is what the CSR is built with. Never the private key itself: see
	// buildCSRFor.
	signer crypto.Signer

	// save commits the key and the issued certificate together, and is the only
	// point at which anything durable is meant to change.
	save func(certPEM, chainPEM string) (*Material, error)

	// discard undoes whatever creating the key already made durable.
	//
	// A no-op on Unix, where creating a key touches nothing. On Windows it
	// deletes the key container, which is the difference between a failing
	// server costing one orphaned container and costing one per attempt
	// forever.
	discard func() error
}

// keyMaker creates the renewal key. Platform-selected at build time: see
// key_other.go and key_windows.go.
//
// predecessorSerial is the serial of the certificate being replaced, empty for
// a first issuance. Unix ignores it; Windows needs it to name the container.
type keyMaker func(dir, deviceID, predecessorSerial string) (*deviceKey, error)

// keyContainerPrefix marks a key container as one this package made.
//
// It is what makes the sweep of superseded certificates safe to run: a
// certificate that arrived on the machine by some other route can name any
// container it likes, and deleting that container because the certificate
// looked like ours would break something we never owned.
const keyContainerPrefix = "everwas-8021x-"

// keyContainerName is the CNG container the renewal key is created in.
//
// Deterministic, and that is the load-bearing part rather than a tidiness
// preference. With a file-based key a failed renewal costs nothing because the
// key never left memory. With CNG the key must be persisted in the provider
// before it can sign the CSR, so it exists before the certificate does, and a
// failed renewal leaves a container behind. Under a random name a server that
// is down for a week would leave one container per attempt in the machine's key
// store, and nothing would ever remove them.
//
// Derived from the device id and from the serial of the certificate being
// REPLACED, which is what makes it both stable and safe:
//
//   - every attempt at the same renewal computes the same name and overwrites
//     its own previous attempt, so a run of failures costs one container rather
//     than one per try
//   - the next renewal starts from a different predecessor serial, so it cannot
//     land on the container holding the key that is currently IN USE.
//     Overwriting that one would take a working device off the network, which
//     is the exact failure the in-memory ordering on Unix exists to prevent
//
// The empty predecessor is a first issuance, which has no live key to collide
// with.
//
// This is a deliberate weakening of the property Ensure otherwise holds. On
// Unix a failed renewal is genuinely free; on Windows it costs one container
// that has to be swept. The device is not harmed either way, because the old
// key and its certificate stay bound and in use, but the two platforms are no
// longer identical and a reader deserves to be told so rather than to find out.
func keyContainerName(deviceID, predecessorSerial string) string {
	// The separator matters: without it a device id ending in "ab" with
	// predecessor "cd" and one ending in "a" with predecessor "bcd" hash the
	// same, and two devices would fight over one container.
	sum := sha256.Sum256([]byte(deviceID + "\x00" + predecessorSerial))
	// Half the digest. Long enough that a collision is not a thing that
	// happens, short enough that the name is readable in certutil output.
	return keyContainerPrefix + fmt.Sprintf("%x", sum[:16])
}
