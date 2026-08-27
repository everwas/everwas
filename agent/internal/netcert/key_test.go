package netcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// certServer is a CA that signs nothing and hands back what it is given, which
// is enough for the orderings under test here.
func certServer(t *testing.T, certPEM, chainPEM string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{
			CertificatePEM: certPEM,
			ChainPEM:       chainPEM,
			Serial:         "01",
		})
	}))
}

// fakeProvider stands in for a CNG key storage provider: a set of persisted
// container names and a record of every name ever created in it.
//
// It exists because the property under test is a design decision, not a Windows
// one, and the machine running these tests has no CNG. What the real provider
// adds is the syscalls; what decides whether containers pile up is the naming
// and the discard ordering, and both of those are here.
type fakeProvider struct {
	live    map[string]bool
	created []string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{live: map[string]bool{}}
}

// make mimics newDeviceKey on Windows: the key is persisted under a
// deterministic name BEFORE it has signed anything, and only discard removes
// it.
func (p *fakeProvider) make(dir, deviceID, predecessorSerial string) (*deviceKey, error) {
	name := keyContainerName(deviceID, predecessorSerial)
	// Overwrite, exactly as NCRYPT_OVERWRITE_KEY_FLAG does: creating a
	// container that exists replaces it rather than making a second one.
	p.live[name] = true
	p.created = append(p.created, name)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &deviceKey{
		signer: key,
		save: func(certPEM, chainPEM string) (*Material, error) {
			return Save(dir, certPEM, chainPEM)
		},
		discard: func() error {
			delete(p.live, name)
			return nil
		},
	}, nil
}

func (p *fakeProvider) distinctCreated() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range p.created {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func TestRepeatedFailedRenewalsDoNotAccumulateKeyContainers(t *testing.T) {
	// The cost of making the key real before the certificate exists. On Unix
	// this is free, because a failed renewal never wrote anything. On Windows
	// the key has to be in the provider to sign the CSR, so a server that is
	// down for a week gets one attempt every hour and every one of them leaves
	// a container. Under a random name that is a hundred and sixty-odd dead
	// keys in the machine's store with nothing that would ever remove them.
	dir := t.TempDir()
	certPEM, chainPEM := selfSigned(t)
	if _, err := Save(dir, certPEM, chainPEM); err != nil {
		t.Fatal(err)
	}

	p := newFakeProvider()
	// Well past the certificate's expiry, so every pass is due for renewal.
	when := time.Now().Add(365 * 24 * time.Hour)
	for i := 0; i < 6; i++ {
		// A server that is not there: the most ordinary renewal failure.
		if _, err := ensure(context.Background(), p.make, dir,
			"http://127.0.0.1:1", "device-1", "secret-secret", when); err == nil {
			t.Fatal("a renewal against an unreachable server reported success")
		}
	}

	if len(p.live) != 0 {
		t.Errorf("%d key containers survived six failed renewals: %v", len(p.live), p.live)
	}
	if got := p.distinctCreated(); len(got) != 1 {
		t.Errorf("six attempts at the same renewal used %d container names, want 1: %v", len(got), got)
	}
}

func TestTheRenewalKeyIsNotCreatedOverTheOneInUse(t *testing.T) {
	// The other half of making the name deterministic. Reusing a name is only
	// safe if it can collide with a previous ATTEMPT and never with the
	// container holding the key the device is currently authenticating with:
	// overwriting that one takes a working machine off the network at its next
	// reauthentication.
	//
	// The generation marker is the serial of the certificate being replaced, so
	// the name moves as soon as a renewal succeeds.
	const device = "device-1"
	first := keyContainerName(device, "")
	second := keyContainerName(device, "0a1b2c")
	third := keyContainerName(device, "0a1b2d")

	if first == second || second == third || first == third {
		t.Errorf("successive generations share a container name: %s %s %s", first, second, third)
	}
	if keyContainerName(device, "0a1b2c") != second {
		t.Error("the same renewal computed two different container names, so a retry would leave the first behind")
	}
	if keyContainerName("device-2", "") == first {
		t.Error("two devices computed the same container name")
	}
}

func TestASuccessfulRenewalKeepsItsKeyContainer(t *testing.T) {
	// The mirror of the accumulation test. discard runs on every path that does
	// not end in an issued certificate, and on the one that does it must not,
	// or the device would hold a certificate whose key it has just deleted.
	dir := t.TempDir()
	p := newFakeProvider()

	certPEM, chainPEM := selfSigned(t)
	srv := certServer(t, certPEM, chainPEM)
	defer srv.Close()

	m, err := ensure(context.Background(), p.make, dir, srv.URL, "device-1", "secret-secret", time.Now())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !m.Issued {
		t.Error("a fresh issuance was not reported as one")
	}
	if len(p.live) != 1 {
		t.Errorf("the issued certificate's key container is %v, want exactly one", p.live)
	}
}
