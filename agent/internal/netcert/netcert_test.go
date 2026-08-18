package netcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTheKeyNeverLeavesAndIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path, err := GenerateKey(dir)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// This key is the machine's identity on the network for the life of the
		// certificate, and unlike a password it cannot be rotated quietly.
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("key mode = %o, want 600", mode)
		}
	}

	csr, err := BuildCSR(path)
	if err != nil {
		t.Fatalf("BuildCSR: %v", err)
	}
	// The only thing that crosses the wire must not carry the private half.
	if block, _ := pem.Decode(csr); block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("csr is not a certificate request: %q", csr)
	}
	if strings.Contains(string(csr), "PRIVATE KEY") {
		t.Error("the CSR contains private key material")
	}
}

func TestTheCsrIsSignedByTheKeyItNames(t *testing.T) {
	// Proof of possession is what stops anyone having a certificate issued for
	// a public key whose private half belongs to somebody else. The server
	// checks it; this makes sure we produce one that survives the check.
	dir := t.TempDir()
	path, err := GenerateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := BuildCSR(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CSR signature does not verify: %v", err)
	}
}

func TestAMissingCertificateIsNamedNotGuessed(t *testing.T) {
	if _, err := Load(t.TempDir()); err != ErrNoCertificate {
		t.Errorf("err = %v, want ErrNoCertificate", err)
	}
}

func TestRenewalStartsAtHalfLifeNotAtExpiry(t *testing.T) {
	// This gap is the entire safety margin. For 802.1X an expired certificate
	// locks the machine off the network with no remote way back, so the weeks
	// between half life and expiry are where retries and alarms have to fit.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &Material{NotBefore: start, NotAfter: start.Add(90 * 24 * time.Hour)}

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"fresh", start.Add(1 * 24 * time.Hour), false},
		{"just before half life", start.Add(44 * 24 * time.Hour), false},
		{"past half life", start.Add(46 * 24 * time.Hour), true},
		{"expired", start.Add(100 * 24 * time.Hour), true},
	} {
		if got := m.DueForRenewal(tc.at); got != tc.want {
			t.Errorf("%s: DueForRenewal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNoCertificateAtAllIsDueForRenewal(t *testing.T) {
	var m *Material
	if !m.DueForRenewal(time.Now()) {
		t.Error("a device with no certificate must be due for one")
	}
}

func TestAnUnstartedCertificateIsDueForRenewal(t *testing.T) {
	// Usually means the clock is wrong. Asking for a fresh one is harmless and
	// is the fastest way to surface it.
	now := time.Now()
	m := &Material{NotBefore: now.Add(48 * time.Hour), NotAfter: now.Add(90 * 24 * time.Hour)}
	if !m.DueForRenewal(now) {
		t.Error("a not-yet-valid certificate should be renewed rather than waited on")
	}
}

func TestSaveWritesTheChainBesideTheCertificate(t *testing.T) {
	dir := t.TempDir()
	// A certificate with no chain beside it is one a supplicant cannot build a
	// path from, and that fails as an authentication rejection rather than as
	// a missing file.
	certPEM, chainPEM := selfSigned(t)
	m, err := Save(dir, certPEM, chainPEM)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, p := range []string{m.CertPath, m.ChainPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", filepath.Base(p), err)
		}
	}
	if m.NotAfter.IsZero() {
		t.Error("expiry was not read from the issued certificate")
	}
}

// selfSigned makes a throwaway certificate so Save can be tested without a CA.
func selfSigned(t *testing.T) (certPEM, chainPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	out := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return out, out + out
}

func TestFindingAnExistingCertificateIsNotReportedAsIssuing(t *testing.T) {
	// The routine twelve-hourly check finds a certificate with weeks left and
	// does nothing. If that were reported as an issuance, the log would show a
	// fresh certificate twice a day forever and the one entry that records a
	// real one would be indistinguishable from the noise.
	dir := t.TempDir()
	certPEM, chainPEM := selfSigned(t)
	if _, err := Save(dir, certPEM, chainPEM); err != nil {
		t.Fatal(err)
	}

	// A server address that would fail if it were contacted at all, so this
	// also pins that a certificate which is not due costs no network.
	m, err := Ensure(context.Background(), dir, "http://127.0.0.1:1", "agent", "secret-secret", time.Now())
	if err != nil {
		t.Fatalf("a certificate that is not due should need no work: %v", err)
	}
	if m.Issued {
		t.Error("finding an existing certificate was reported as issuing a new one")
	}
}

func TestAFailedRenewalLeavesTheWorkingCertificateAlone(t *testing.T) {
	// The ordering that matters most here. Writing the new key before the
	// certificate for it exists is the obvious shape, and on a failed renewal
	// it leaves the key on disk not matching the certificate on disk: the
	// machine drops off the network at its next reauthentication, having been
	// healthy until it tried to renew. Renewal must never leave a device worse
	// than not renewing.
	dir := t.TempDir()
	keyPath, err := GenerateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, chainPEM := selfSigned(t)
	if _, err := Save(dir, certPEM, chainPEM); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// A server that is not there: the most ordinary renewal failure.
	_, err = Ensure(context.Background(), dir, "http://127.0.0.1:1", "agent", "secret-secret", time.Now().Add(365*24*time.Hour))
	if err == nil {
		t.Fatal("a renewal against an unreachable server reported success")
	}

	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("the key is gone after a failed renewal: %v", err)
	}
	if string(before) != string(after) {
		t.Error("a failed renewal replaced the key, so it no longer matches the certificate " +
			"on disk and the device will fail its next authentication")
	}
}
