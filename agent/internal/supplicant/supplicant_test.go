package supplicant

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testIdentity = "01a00b45-0e50-78c8-b572-8b8fbc272ad1"

func wired() Profile {
	return Profile{Identity: testIdentity, CertDir: "/etc/everwas/netcert"}
}

func TestWiredProfileSetsApScanZero(t *testing.T) {
	// The single most common reason a hand-written wired profile silently
	// never authenticates. Without it wpa_supplicant scans for access points,
	// finds none on a wired interface, and sits there having done nothing
	// wrong and nothing at all. There is no error to read.
	out, err := Render(wired())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ap_scan=0") {
		t.Error("a wired profile without ap_scan=0 will never authenticate, silently")
	}
}

func TestWiredAndWirelessUseDifferentKeyManagement(t *testing.T) {
	// Using the wireless value on a wired interface fails in a way that reads
	// like a certificate problem, which is a long way from the cause.
	w, err := Render(wired())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w, "key_mgmt=IEEE8021X") {
		t.Error("wired profile does not use IEEE8021X")
	}
	if strings.Contains(w, "WPA-EAP") {
		t.Error("wired profile used the wireless key management mode")
	}

	p := wired()
	p.SSID = "CORP-SECURE"
	wl, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wl, "key_mgmt=WPA-EAP") {
		t.Error("wireless profile does not use WPA-EAP")
	}
	if !strings.Contains(wl, `ssid="CORP-SECURE"`) {
		t.Error("wireless profile does not name the network")
	}
	if strings.Contains(wl, "ap_scan=0") {
		t.Error("ap_scan=0 on a wireless profile stops it scanning for the network it needs")
	}
}

func TestTheProfilePointsAtTheCertificateTheAgentWrote(t *testing.T) {
	out, err := Render(wired())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ca_cert="/etc/everwas/netcert/network-chain.pem"`,
		`client_cert="/etc/everwas/netcert/network.crt"`,
		`private_key="/etc/everwas/netcert/network.key"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("profile is missing %s", want)
		}
	}
	// The chain, not the leaf, as the trust anchor. Pointing ca_cert at the
	// device's own certificate is a plausible-looking mistake that fails the
	// handshake with an unhelpful error.
	if strings.Contains(out, `ca_cert="/etc/everwas/netcert/network.crt"`) {
		t.Error("ca_cert points at the device certificate rather than the chain")
	}
}

func TestThePrivateKeyIsReferencedNeverInlined(t *testing.T) {
	// The key is protected by file permissions where the agent wrote it.
	// Copying its contents into a config file would defeat that entirely, and
	// this file is one an operator is likely to paste into a ticket.
	out, err := Render(wired())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Fatal("the generated profile contains private key material")
	}
}

func TestAQuoteInAValueIsRefusedRatherThanEscaped(t *testing.T) {
	// wpa_supplicant has no escape mechanism worth relying on, so a value
	// containing a quote does not produce a broken string, it produces
	// additional directives. An SSID could append an open network to the
	// machine's configuration.
	for _, tc := range []struct {
		name    string
		profile Profile
	}{
		{"ssid closes the string and opens a network block", Profile{
			Identity: testIdentity, CertDir: "/etc/everwas/netcert",
			SSID: "corp\"\nnetwork={ key_mgmt=NONE",
		}},
		{"identity carries a newline", Profile{
			Identity: "device\nap_scan=1", CertDir: "/etc/everwas/netcert",
		}},
		{"cert dir carries a quote", Profile{
			Identity: testIdentity, CertDir: "/etc/\"everwas",
		}},
	} {
		out, err := Render(tc.profile)
		if !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("%s: err = %v, want ErrInvalidProfile", tc.name, err)
		}
		if out != "" {
			t.Errorf("%s: returned a config despite refusing it", tc.name)
		}
	}
}

func TestAnEmptyIdentityIsRefused(t *testing.T) {
	// An empty identity produces a config that authenticates but appears in
	// the RADIUS log as nothing, so a session cannot be tied to a device.
	if _, err := Render(Profile{CertDir: "/etc/everwas/netcert"}); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("err = %v, want ErrInvalidProfile", err)
	}
}

func TestWriteReplacesTheProfileAtomically(t *testing.T) {
	dir := t.TempDir()
	path, err := Write(dir, wired())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != FileName {
		t.Errorf("wrote %s, want %s", filepath.Base(path), FileName)
	}

	// Rewriting must not leave the temporary file behind: a stray .tmp beside
	// a config directory is the kind of thing that later gets globbed.
	if _, err := Write(dir, wired()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want 1", len(entries))
	}
}

func TestARefusedProfileWritesNothing(t *testing.T) {
	// A validation failure must not leave a partial or previous-generation
	// config on disk that something might later apply.
	dir := t.TempDir()
	_, err := Write(dir, Profile{Identity: "bad\"identity", CertDir: "/etc/everwas/netcert"})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("err = %v, want ErrInvalidProfile", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a refused profile still wrote %d file(s)", len(entries))
	}
}
