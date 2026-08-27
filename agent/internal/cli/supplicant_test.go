package cli

import (
	"strings"
	"testing"
)

func TestWindowsIsToldToStartWiredAutoConfigFirst(t *testing.T) {
	// Verified on Windows 11: dot3svc ships Stopped and Manual, and netsh
	// answers "The Wired AutoConfig Service (dot3svc) is not running." rather
	// than adding the profile. An operator handed only the netsh line meets
	// that message with nothing telling them it was expected.
	steps := strings.Join(applySteps("windows", `C:\ProgramData\Everwas\Agent\everwas-8021x.xml`), "\n")
	for _, want := range []string{"Start-Service dot3svc", "netsh lan add profile", "everwas-8021x.xml"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the Windows instructions do not mention %q:\n%s", want, steps)
		}
	}
	if strings.Contains(steps, "wpa_supplicant") {
		t.Error("Windows was told to run wpa_supplicant, which is not on the machine")
	}
	// Order matters: netsh refuses until the service is up.
	if i, j := strings.Index(steps, "dot3svc"), strings.Index(steps, "netsh lan add"); i > j {
		t.Error("the netsh step comes before the service it depends on")
	}
}

func TestUnixIsStillToldToRunWpaSupplicant(t *testing.T) {
	steps := strings.Join(applySteps("linux", "/etc/everwas/wpa_supplicant-everwas.conf"), "\n")
	if !strings.Contains(steps, "wpa_supplicant -c /etc/everwas/wpa_supplicant-everwas.conf") {
		t.Errorf("the Linux instructions changed: %s", steps)
	}
	if strings.Contains(steps, "netsh") {
		t.Error("netsh leaked into the Linux instructions")
	}
}
