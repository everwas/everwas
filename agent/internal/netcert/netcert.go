// Package netcert obtains and holds the device's NETWORK certificate, the one
// presented for 802.1X EAP-TLS.
//
// Deliberately separate from the agent's management credential. If one thing
// authenticated both, revoking a device's network access would simultaneously
// revoke the ability to reach it and fix it: a self-inflicted truck roll at the
// exact moment access matters most. Management stays on wss/443, which a
// quarantine VLAN usually permits, so a device that fails 802.1X can still be
// repaired. See docs/adr/0003.
//
// The private key is generated HERE and never leaves. Only a CSR crosses the
// wire. A key that travelled is a key that sat in a log, a proxy buffer and
// somebody's database, and unlike a password it cannot be rotated quietly: it
// is the machine's identity on the network until the certificate expires.
//
// Where the key lives differs by platform, and on Windows it differs for two
// reasons rather than one. It is a CNG key storage provider container, so the
// key is never bytes this process can read, and it is in the machine
// certificate store, because the native Windows supplicant takes its client
// credential from there and cannot be pointed at a file. Unix writes a PEM and
// the supplicant is given its path. See key.go for the seam and key_windows.go
// for what Windows does with it.
package netcert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/everwas/everwas/agent/internal/secure"
)

// RenewAt is the fraction of a certificate's life after which renewal starts.
//
// Half, not "nearly expired", and the gap is the entire safety margin. For
// 802.1X an expired certificate locks the machine off the network, and a
// machine that cannot reach the network cannot be repaired remotely: that is a
// physical visit. Forty-five days of a ninety-day certificate is where failed
// attempts, alarms and somebody noticing all have to fit.
const RenewAt = 0.5

// UrgentAt is where renewal stops being routine, borrowed from DHCP's
// rebinding timer T2 at 87.5% of a lease.
//
// The point of T2 in DHCP is not that it is another retry: it is that the
// client CHANGES STRATEGY, having concluded the normal path is broken. Without
// it a device one day past half life and a device one hour from expiry get
// treated identically, retried at the same rate and logged at the same
// severity, when one is routine and the other is about to fall off the
// network.
//
// Where the analogy stops: DHCP rebinding means ask a DIFFERENT server,
// because DHCP assumes several interchangeable ones. There is exactly one CA
// here and nothing to rebind to, so past this point the agent can only try
// harder and say so louder, over the NATS link, which is a separate channel
// from the HTTPS one that is failing.
const UrgentAt = 0.875

// RenewJitter spreads a fleet's renewals so they do not arrive together.
//
// A hundred machines imaged on the same afternoon hold certificates whose half
// lives fall within the same few hours, and without this they stampede the CA
// in one window. The offset is derived from the device's own id, so it is
// stable: the same machine picks the same point every time rather than
// wobbling, which is the same deterministic-jitter approach the scheduler uses
// to spread a fleet across a patch window.
//
// Five percent of a ninety-day certificate is four and a half days of spread,
// taken out of the forty-five days of margin, which is a rounding error
// against what it buys.
const RenewJitter = 0.05

const (
	keyFileName   = "network.key"
	certFileName  = "network.crt"
	chainFileName = "network-chain.pem"
)

// ErrNoCertificate means this device has not been issued one yet.
var ErrNoCertificate = errors.New("netcert: no certificate on this device")

// Material is what the device holds: its key, its certificate, and the chain.
type Material struct {
	CertPath  string
	KeyPath   string
	ChainPath string
	NotAfter  time.Time
	NotBefore time.Time

	// Serial identifies this exact certificate, lowercase hex, matching the
	// form the server records. Reported in the heartbeat so the server can
	// tell what the device is ACTUALLY holding rather than what it was last
	// issued, which are different whenever a renewal half-failed or a machine
	// came back from a backup image.
	Serial string

	// Issued reports that THIS call obtained the certificate, as opposed to
	// finding one already on disk. Without it the caller cannot tell the two
	// apart, and the choice is between logging every routine check (so the one
	// line that records a real issuance is buried) and logging nothing (so the
	// event that governs whether this machine can reach the network leaves no
	// trace at all).
	Issued bool
}

// GenerateKey creates the device's private key and writes it, owner-only.
//
// P-256 rather than RSA: every 802.1X supplicant in use handles it, the
// handshake is cheaper on the switch, and the key is small enough to sit in a
// TPM without special handling.
//
// This is the UNIX path. On Windows the key is created inside a CNG key storage
// provider instead and no file is written at all: see key_windows.go, and note
// that GenerateKey itself is no longer on the Windows renewal path.
//
// On Unix the file permissions below are the only thing protecting it, which
// means a stolen disk yields a usable network identity for the life of the
// certificate. That is the reason the lifetime is ninety days and not a year.
// The non-exportable answer here is tpm2-pkcs11 on Linux and the Secure Enclave
// on macOS, and neither is done.
func GenerateKey(dir string) (string, error) {
	// secure, not os.MkdirAll: on Windows the 0700 is ignored and the
	// directory inherits C:\ProgramData's default, which grants every local
	// user read. This directory holds the machine's network identity.
	if err := secure.MkdirAll(dir); err != nil {
		return "", err
	}
	_, pemBytes, err := newKeyPair()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, keyFileName)
	if err := writeKey(path, pemBytes); err != nil {
		return "", err
	}
	return path, nil
}

// newKeyPair makes a key and its PEM without touching disk, so a caller can
// obtain a certificate for it BEFORE committing anything.
func newKeyPair() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("netcert: generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("netcert: marshal key: %w", err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// buildCSRFor takes a crypto.Signer rather than a private key, and that seam is
// what lets the key live somewhere this package cannot read.
// x509.CreateCertificateRequest never wanted the private half: it wants a public
// key and something that will sign one digest. A key inside a CNG provider, or
// later inside a TPM, satisfies that without any of the code below changing.
func buildCSRFor(key crypto.Signer) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "everwas-device"},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("netcert: build csr: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// saveAll commits a matched key, certificate and chain together.
func saveAll(dir string, keyPEM []byte, certPEM, chainPEM string) (*Material, error) {
	if err := secure.MkdirAll(dir); err != nil {
		return nil, err
	}
	// Key first: a certificate whose key is missing is useless, while a key
	// whose certificate is missing is merely unused, and the next Ensure
	// replaces it.
	if err := writeKey(filepath.Join(dir, keyFileName), keyPEM); err != nil {
		return nil, err
	}
	return Save(dir, certPEM, chainPEM)
}

func writeKey(path string, pemBytes []byte) error {
	// 0600 before anything is written, not after: a key that exists
	// world-readable for even an instant has been readable.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("netcert: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(pemBytes); err != nil {
		return fmt.Errorf("netcert: write key: %w", err)
	}
	// fsync: this key matches a certificate the server has already issued, and
	// losing it to a power cut leaves the device holding a certificate it
	// cannot use while the server believes it can.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("netcert: sync key: %w", err)
	}
	return nil
}

// BuildCSR makes a signing request for the key at keyPath.
//
// The subject is a placeholder on purpose. The server sets the real identity
// from the credential the agent authenticated with, because a CSR is
// attacker-controlled input from its point of view: a device asks for a key to
// be signed, it does not choose whose identity that key carries.
func BuildCSR(keyPath string) ([]byte, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("netcert: read key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("netcert: key file is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("netcert: parse key: %w", err)
	}

	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "everwas-device"},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("netcert: build csr: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Save writes an issued certificate and its chain beside the key.
func Save(dir, certPEM, chainPEM string) (*Material, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, certFileName)
	chainPath := filepath.Join(dir, chainFileName)

	// The chain first. A certificate on disk with no chain beside it is one a
	// supplicant cannot build a path from, and the failure appears as an
	// authentication rejection rather than as a missing file.
	if err := writeFile(chainPath, []byte(chainPEM), 0o644); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, []byte(certPEM), 0o644); err != nil {
		return nil, err
	}
	return &Material{
		CertPath:  certPath,
		KeyPath:   filepath.Join(dir, keyFileName),
		ChainPath: chainPath,
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
		Serial:    serialHex(cert),
	}, nil
}

// Load reports what this device currently holds.
func Load(dir string) (*Material, error) {
	certPath := filepath.Join(dir, certFileName)
	raw, err := os.ReadFile(certPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCertificate
	}
	if err != nil {
		return nil, fmt.Errorf("netcert: read certificate: %w", err)
	}
	cert, err := parseCert(string(raw))
	if err != nil {
		return nil, err
	}
	return &Material{
		CertPath:  certPath,
		KeyPath:   filepath.Join(dir, keyFileName),
		ChainPath: filepath.Join(dir, chainFileName),
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
		Serial:    serialHex(cert),
	}, nil
}

// Phase is where a certificate sits in its life, which decides how hard the
// agent tries and how loudly it complains.
type Phase int

const (
	// PhaseFresh: nothing to do.
	PhaseFresh Phase = iota
	// PhaseDue: past the renewal point, with most of the margin still ahead.
	// Failures here are ordinary and worth a warning, no more.
	PhaseDue
	// PhaseUrgent: past UrgentAt. Renewal has been failing for most of the
	// margin and the deadline is real. Retry harder, log louder, and tell
	// somebody who can act.
	PhaseUrgent
	// PhaseExpired: the certificate no longer authenticates. Under ADR-0004
	// the device drops to a remediation VLAN it can still reach the server
	// from, so this is recoverable rather than terminal, but it is an outage.
	PhaseExpired
)

func (p Phase) String() string {
	switch p {
	case PhaseFresh:
		return "fresh"
	case PhaseDue:
		return "due"
	case PhaseUrgent:
		return "urgent"
	case PhaseExpired:
		return "expired"
	}
	return "unknown"
}

// renewalOffset is the per-device share of RenewJitter, in [0, RenewJitter).
//
// Derived from the device id so it is STABLE. A random offset re-rolled each
// time would let a device drift earlier on one check and later on the next,
// which is both harder to reason about and pointless: the goal is to separate
// devices from each other, not to separate a device from itself.
// SHA-256 rather than FNV, and not for cryptographic reasons: FNV-1a
// multiplies after each byte, so the bytes consumed LAST move mainly the low
// bits and barely disturb the high ones. Agent ids are UUIDv7, which are
// time-ordered, so machines enrolled the same afternoon share a long prefix and
// differ only in the tail. Under FNV they landed on nearly the same offset,
// which is no spread at all for precisely the mass-deployment fleet this
// exists to spread. Measured, not assumed: 200 such ids filled 6 of 10 buckets.
//
// hash/maphash would be worse still: it is seeded per process, so a device
// would pick a different offset every time the agent restarted.
func renewalOffset(id string) float64 {
	sum := sha256.Sum256([]byte(id))
	n := binary.BigEndian.Uint32(sum[:4])
	return float64(n) / float64(math.MaxUint32) * RenewJitter
}

// PhaseAt reports the certificate's phase for a given device.
//
// nil, or a certificate whose validity window makes no sense, is PhaseExpired:
// the most urgent answer, because a device holding nothing usable is the case
// this exists to escalate.
func (m *Material) PhaseAt(now time.Time, id string) Phase {
	if m == nil {
		return PhaseExpired
	}
	life := m.NotAfter.Sub(m.NotBefore)
	if life <= 0 {
		return PhaseExpired
	}
	if !now.Before(m.NotAfter) {
		return PhaseExpired
	}
	if now.Before(m.NotBefore) {
		// Not valid yet, which almost always means the clock is wrong. Treated
		// as due rather than fresh so the agent asks for a replacement, which
		// is both harmless and the fastest way to surface it. Not urgent: a
		// wrong clock is not a deadline.
		return PhaseDue
	}

	elapsed := now.Sub(m.NotBefore).Seconds() / life.Seconds()
	switch {
	case elapsed >= UrgentAt:
		return PhaseUrgent
	case elapsed >= RenewAt+renewalOffset(id):
		return PhaseDue
	default:
		return PhaseFresh
	}
}

// DueForRenewal reports whether the certificate is past RenewAt of its life.
//
// Also true for a certificate that is already expired or not yet valid. An
// expired one is the emergency this exists to prevent; one whose validity has
// not started usually means the clock is wrong, and asking for a fresh one is
// both harmless and the fastest way to surface it.
func (m *Material) DueForRenewal(now time.Time) bool {
	if m == nil {
		return true
	}
	life := m.NotAfter.Sub(m.NotBefore)
	if life <= 0 {
		return true
	}
	if now.Before(m.NotBefore) {
		// Not valid yet. Almost always a wrong clock, and the negative elapsed
		// time below would otherwise read as "brand new, nothing to do" for
		// however long the skew lasts, which is precisely the machine that is
		// about to fail authentication.
		return true
	}
	elapsed := now.Sub(m.NotBefore)
	return elapsed >= time.Duration(float64(life)*RenewAt)
}

// serialHex renders a serial the same way the server records it: lowercase
// hex with no leading zeros, so the two can be compared as strings without
// either side needing to know how the other formats a big integer.
func serialHex(cert *x509.Certificate) string {
	return fmt.Sprintf("%x", cert.SerialNumber)
}

func parseCert(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("netcert: certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("netcert: parse certificate: %w", err)
	}
	return cert, nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("netcert: write %s: %w", path, err)
	}
	return nil
}
