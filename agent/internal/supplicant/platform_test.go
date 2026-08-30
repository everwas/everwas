package supplicant

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// testIssuerPin stands in for the SHA-1 thumbprint of our issuing CA.
const testIssuerPin = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4"

func TestWindowsGetsTheProfileWindowsCanRead(t *testing.T) {
	// The gap this closes. RenderWindows was written, tested and checked
	// against real netsh, and Write never called it: there was no platform
	// branch, so `everwas-agent supplicant-profile` on Windows produced a
	// wpa_supplicant config, which nothing on that machine reads. The renderer
	// was tested; the choice of renderer was not.
	name, out, err := renderFor("windows", Profile{
		Identity: "device-1", CertDir: `C:\ProgramData\Everwas\Agent\netcert`,
		ClientIssuerThumbprint: testIssuerPin,
	})
	if err != nil {
		t.Fatalf("renderFor windows: %v", err)
	}
	if !strings.HasSuffix(name, ".xml") {
		t.Errorf("filename is %q; netsh reads an XML document and the extension is how an operator knows", name)
	}
	var v any
	if err := xml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("Windows was handed something that is not XML: %v", err)
	}
	if !strings.Contains(out, "<LANProfile") {
		t.Error("not a LAN profile, so netsh lan add profile would refuse it")
	}
	if strings.Contains(out, "wpa_supplicant") || strings.Contains(out, "key_mgmt") {
		t.Error("the wpa_supplicant config leaked into the Windows path")
	}
}

func TestUnixStillGetsWpaSupplicant(t *testing.T) {
	name, out, err := renderFor("linux", Profile{Identity: "device-1", CertDir: "/etc/everwas/netcert"})
	if err != nil {
		t.Fatalf("renderFor linux: %v", err)
	}
	if name != FileName {
		t.Errorf("filename = %q, want %q", name, FileName)
	}
	if !strings.Contains(out, "key_mgmt=IEEE8021X") {
		t.Error("the wired wpa_supplicant profile changed shape")
	}
}

func TestWirelessOnWindowsIsRefusedRatherThanRenderedWrong(t *testing.T) {
	// A wireless profile on Windows is a WLANProfile with its own schema, added
	// by `netsh wlan`. Emitting the wired document for an SSID would produce a
	// file that netsh accepts nowhere, and the error would name the file rather
	// than the request.
	_, _, err := renderFor("windows", Profile{
		Identity: "device-1", CertDir: `C:\netcert`, SSID: "corp",
		ClientIssuerThumbprint: testIssuerPin,
	})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("err = %v, want ErrInvalidProfile", err)
	}
}

func TestTheWindowsProfileThatIsWrittenCarriesTheClientPin(t *testing.T) {
	// The pin's own history is the reason this test exists. The CAHashList
	// filter was added to RenderWindows, tested, and verified against real
	// netsh — and renderFor kept calling RenderWindows with an empty
	// WindowsProfile, so every profile the command actually wrote rendered
	// with SimpleCertSelection true and Windows free to present the
	// enterprise CA's certificate instead of ours. The renderer was tested;
	// what reached the renderer was not. Same failure renderFor's comment
	// describes, one level down.
	_, out, err := renderFor("windows", Profile{
		Identity: "device-1", CertDir: `C:\ProgramData\Everwas\Agent\netcert`,
		ClientIssuerThumbprint: "A1 B2 C3 D4 E5 F6 07 18 29 3A 4B 5C 6D 7E 8F 90 A1 B2 C3 D4",
	})
	if err != nil {
		t.Fatalf("renderFor windows: %v", err)
	}
	if !strings.Contains(out, "<IssuerHash>"+testIssuerPin+"</IssuerHash>") {
		t.Errorf("the issuer pin did not reach the written profile:\n%s", out)
	}
	if !strings.Contains(out, "<SimpleCertSelection>false</SimpleCertSelection>") {
		t.Error("the profile still lets Windows choose which certificate to present")
	}
}

func TestWindowsWithNoPinIsRefusedNotRenderedUnpinned(t *testing.T) {
	// RenderWindows accepts an empty thumbprint list as a documented
	// fallback, and that acceptance is precisely what let the pin vanish
	// between the command and the file without anything failing. The command
	// path refuses instead: an error names the missing piece, an unpinned
	// profile authenticates with whatever certificate Windows fancies and
	// fails at the RADIUS server with nothing pointing here.
	_, _, err := renderFor("windows", Profile{
		Identity: "device-1", CertDir: `C:\ProgramData\Everwas\Agent\netcert`,
	})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("err = %v, want ErrInvalidProfile", err)
	}
}

func TestWindowsRefusesTheSameBadInputAsUnix(t *testing.T) {
	// An operator who tests a profile on Linux and deploys it on Windows should
	// not meet a different set of rules.
	for _, p := range []Profile{
		{CertDir: "/etc/everwas/netcert"},
		{Identity: "device\nap_scan=1", CertDir: "/etc/everwas/netcert"},
	} {
		if _, _, err := renderFor("windows", p); !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("%+v: err = %v, want ErrInvalidProfile", p, err)
		}
	}
}
