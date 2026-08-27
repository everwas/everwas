package supplicant

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

func TestWindowsGetsTheProfileWindowsCanRead(t *testing.T) {
	// The gap this closes. RenderWindows was written, tested and checked
	// against real netsh, and Write never called it: there was no platform
	// branch, so `everwas-agent supplicant-profile` on Windows produced a
	// wpa_supplicant config, which nothing on that machine reads. The renderer
	// was tested; the choice of renderer was not.
	name, out, err := renderFor("windows", Profile{
		Identity: "device-1", CertDir: `C:\ProgramData\Everwas\Agent\netcert`,
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
