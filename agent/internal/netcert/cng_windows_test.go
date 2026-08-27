//go:build windows

package netcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// These tests touch the MACHINE key store and the MACHINE certificate store of
// the box they run on. That is the only place the behaviour being checked
// exists, and it is also why they do not run by default: a developer running
// `go test ./...` on their own workstation should not find keys and
// certificates appearing in LocalMachine.
//
// Run them deliberately, elevated:
//
//	set EVERWAS_WINDOWS_KEYSTORE_TEST=1
//	go test ./internal/netcert/ -run Windows -v
//
// Set EVERWAS_WINDOWS_KEYSTORE_KEEP=1 as well to leave the artefacts behind for
// inspection with `certutil -store My` and `certutil -key -csp "Microsoft
// Software Key Storage Provider"`.
func requireKeyStore(t *testing.T) {
	t.Helper()
	if os.Getenv("EVERWAS_WINDOWS_KEYSTORE_TEST") == "" {
		t.Skip("set EVERWAS_WINDOWS_KEYSTORE_TEST=1 to run against the real machine key store")
	}
}

func keepArtefacts() bool { return os.Getenv("EVERWAS_WINDOWS_KEYSTORE_KEEP") != "" }

// testCA signs whatever it is asked to, after checking the request proves
// possession. The check is the point: a CSR whose signature does not verify is
// exactly what a wrong r||s to DER conversion produces, and it produces it
// silently.
type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	der  []byte
	cn   string

	serial   int64
	verified bool
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "everwas test CA (delete me)"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{key: key, cert: cert, der: der, cn: cn, serial: 100}
}

func (ca *testCA) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		block, _ := pem.Decode([]byte(req.CsrPEM))
		if block == nil {
			t.Error("the agent sent something that is not PEM")
			http.Error(w, "bad csr", http.StatusBadRequest)
			return
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Errorf("parse csr: %v", err)
			http.Error(w, "bad csr", http.StatusBadRequest)
			return
		}
		// The whole reason for the ASN.1 re-encoding in cngSigner.Sign. NCrypt
		// hands back r||s; anything that reads a CSR expects the DER SEQUENCE.
		if err := csr.CheckSignature(); err != nil {
			t.Errorf("the CSR signed by CNG does not verify: %v", err)
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		ca.verified = true

		ca.serial++
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(ca.serial),
			Subject:      pkix.Name{CommonName: ca.cn},
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(2 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		leaf, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
		if err != nil {
			t.Errorf("sign: %v", err)
			http.Error(w, "sign failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{
			CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf})),
			ChainPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der})),
			Serial:         fmt.Sprintf("%x", ca.serial),
		})
	}))
}

func TestWindowsTheKeyIsMadeInCNGAndTheCertificateIsBoundToIt(t *testing.T) {
	requireKeyStore(t)

	dir := t.TempDir()
	// A machine upgraded from the file-based build arrives with this. Leaving
	// it would mean the key stopped being on disk for new installs and not for
	// the ones that have been running longest.
	if _, err := GenerateKey(dir); err != nil {
		t.Fatal(err)
	}

	cn := fmt.Sprintf("everwas-probe-%d", time.Now().Unix())
	ca := newTestCA(t, cn)
	srv := ca.serve(t)
	defer srv.Close()

	deviceID := cn
	m, err := ensure(context.Background(), newDeviceKey, dir, srv.URL, deviceID, "secret-secret", time.Now())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !keepArtefacts() {
		defer removeProbeArtefacts(t, cn)
	}
	if !m.Issued {
		t.Fatal("a first issuance was not reported as one")
	}
	if !ca.verified {
		t.Fatal("the CA never checked a signature, so this proved nothing")
	}

	// No key file, including the one that was there before. The credential
	// lives in the provider and nowhere else, which is the entire point of the
	// exercise.
	if _, err := os.Stat(m.KeyPath); err == nil {
		t.Errorf("%s exists: the private key is still bytes on disk", m.KeyPath)
	}

	container := keyContainerName(deviceID, "")
	t.Logf("container: %s", container)
	t.Logf("subject:   CN=%s", cn)
	t.Logf("serial:    %s", m.Serial)

	// The binding is what the supplicant reads. Without it the certificate sits
	// in the store looking correct and is never used.
	found := findInMyStore(t, cn, m.Serial)
	if found == "" {
		t.Fatal("the issued certificate is not in LocalMachine\\My")
	}
	if found != container {
		t.Errorf("the certificate names container %q, want %q", found, container)
	}

	// And Windows agrees it can reach the key, which is a stronger statement
	// than the property merely being set to the right string.
	if !hasUsablePrivateKey(t, cn, m.Serial) {
		t.Error("Windows cannot acquire the private key for the certificate it just stored")
	}
}

func TestWindowsARenewalSweepsTheCertificateAndKeyItReplaced(t *testing.T) {
	requireKeyStore(t)

	dir := t.TempDir()
	cn := fmt.Sprintf("everwas-probe-sweep-%d", time.Now().Unix())
	ca := newTestCA(t, cn)
	srv := ca.serve(t)
	defer srv.Close()

	deviceID := cn
	first, err := ensure(context.Background(), newDeviceKey, dir, srv.URL, deviceID, "secret-secret", time.Now())
	if err != nil {
		t.Fatalf("first issuance: %v", err)
	}
	if !keepArtefacts() {
		defer removeProbeArtefacts(t, cn)
	}
	firstContainer := keyContainerName(deviceID, "")

	// Far enough forward that the two-hour certificate is due.
	second, err := ensure(context.Background(), newDeviceKey, dir, srv.URL, deviceID, "secret-secret",
		time.Now().Add(90*time.Minute))
	if err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if second.Serial == first.Serial {
		t.Fatal("the renewal did not produce a new certificate")
	}
	secondContainer := keyContainerName(deviceID, first.Serial)
	t.Logf("first  container %s serial %s", firstContainer, first.Serial)
	t.Logf("second container %s serial %s", secondContainer, second.Serial)

	if firstContainer == secondContainer {
		t.Fatal("the renewal reused the container holding the key still in use")
	}
	if findInMyStore(t, cn, second.Serial) != secondContainer {
		t.Error("the renewed certificate is not bound to its own container")
	}
	if got := findInMyStore(t, cn, first.Serial); got != "" {
		t.Errorf("the superseded certificate is still in the store (container %q)", got)
	}
	if containerExists(t, firstContainer) {
		t.Error("the superseded key container survived the renewal, so every renewal leaks one")
	}
	if !containerExists(t, secondContainer) {
		t.Error("the container for the certificate now in use was deleted")
	}
}

func TestWindowsAFailedRenewalLeavesNoOrphanedContainer(t *testing.T) {
	requireKeyStore(t)

	dir := t.TempDir()
	deviceID := fmt.Sprintf("everwas-probe-fail-%d", time.Now().Unix())
	container := keyContainerName(deviceID, "")
	defer func() { _ = deleteCNGKey(container) }()

	// A server that is not there: the most ordinary renewal failure, and the
	// one that repeats every hour for as long as it lasts.
	for i := 0; i < 3; i++ {
		if _, err := ensure(context.Background(), newDeviceKey, dir,
			"http://127.0.0.1:1", deviceID, "secret-secret", time.Now()); err == nil {
			t.Fatal("a renewal against an unreachable server reported success")
		}
		if containerExists(t, container) {
			t.Fatalf("attempt %d left its key container behind", i+1)
		}
	}
}

// findInMyStore returns the key container LocalMachine\My has this
// certificate bound to, or "" if the certificate is not there.
//
// Matched on subject AND serial. Serial alone looked sufficient and is not:
// two runs of these tests stand up two throwaway CAs that both start counting
// from the same number, so a leftover certificate from an earlier run answers
// for the one being looked for and the sweep appears not to have happened.
func findInMyStore(t *testing.T, cn, serial string) string {
	t.Helper()
	store, err := openMachineStore(storeMy)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CertCloseStore(store, 0)

	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || ctx == nil {
			return ""
		}
		prev = ctx
		cert, err := parseContext(ctx)
		if err != nil {
			continue
		}
		if cert.Subject.CommonName == cn && serialHex(cert) == serial {
			return contextKeyContainer(ctx)
		}
	}
}

// hasUsablePrivateKey asks Windows to actually acquire the key, rather than
// asking whether a property happens to be set.
func hasUsablePrivateKey(t *testing.T, cn, serial string) bool {
	t.Helper()
	store, err := openMachineStore(storeMy)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CertCloseStore(store, 0)

	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || ctx == nil {
			return false
		}
		prev = ctx
		cert, err := parseContext(ctx)
		if err != nil || cert.Subject.CommonName != cn || serialHex(cert) != serial {
			continue
		}
		var key windows.Handle
		var spec uint32
		var mustFree bool
		// CRYPT_ACQUIRE_ONLY_NCRYPT_KEY_FLAG: refuse a legacy CryptoAPI handle,
		// so a pass here means Windows found the CNG key specifically.
		const onlyNCrypt = 0x00040000
		if err := windows.CryptAcquireCertificatePrivateKey(
			ctx, onlyNCrypt, nil, &key, &spec, &mustFree,
		); err != nil {
			t.Logf("CryptAcquireCertificatePrivateKey: %v", err)
			return false
		}
		if mustFree {
			freeNCryptObject(key)
		}
		return true
	}
}

func containerExists(t *testing.T, name string) bool {
	t.Helper()
	prov, key, found, err := openMachineKey(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	freeNCryptObject(key)
	freeNCryptObject(prov)
	return found
}

// removeProbeArtefacts takes the machine back to how it was found: the test
// certificates out of My and Root, and their key containers out of the
// provider.
func removeProbeArtefacts(t *testing.T, cn string) {
	t.Helper()
	for _, storeName := range []string{storeMy, storeRoot, storeCA} {
		store, err := openMachineStore(storeName)
		if err != nil {
			t.Logf("cleanup: %v", err)
			continue
		}
		var doomed []*windows.CertContext
		var containers []string
		var prev *windows.CertContext
		for {
			ctx, err := windows.CertEnumCertificatesInStore(store, prev)
			if err != nil || ctx == nil {
				break
			}
			prev = ctx
			cert, err := parseContext(ctx)
			if err != nil {
				continue
			}
			if cert.Subject.CommonName != cn && cert.Subject.CommonName != "everwas test CA (delete me)" {
				continue
			}
			containers = append(containers, contextKeyContainer(ctx))
			doomed = append(doomed, windows.CertDuplicateCertificateContext(ctx))
		}
		for i, ctx := range doomed {
			_ = windows.CertDeleteCertificateFromStore(ctx)
			if containers[i] != "" {
				_ = deleteCNGKey(containers[i])
			}
		}
		windows.CertCloseStore(store, 0)
	}
}
