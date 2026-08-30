//go:build !windows

package netcert

import (
	"errors"
	"testing"
)

func TestPresenceOnUnixIsTheFileCheck(t *testing.T) {
	// wpa_supplicant genuinely reads these files, so on Unix their absence
	// means there is nothing to present, whatever any store somewhere else
	// might hold.
	if _, err := Presence(t.TempDir(), "device-1"); !errors.Is(err, ErrNoCertificate) {
		t.Errorf("empty dir: err = %v, want ErrNoCertificate", err)
	}
}

func TestPresenceOnUnixNamesNoIssuerToPin(t *testing.T) {
	dir := t.TempDir()
	certPEM, chainPEM := selfSigned(t)
	if _, err := Save(dir, certPEM, chainPEM); err != nil {
		t.Fatal(err)
	}
	pin, err := Presence(dir, "device-1")
	if err != nil {
		t.Fatalf("Presence with material on disk: %v", err)
	}
	if pin != "" {
		// The pin is a Windows concept: wpa_supplicant presents the file it
		// is pointed at and cannot present anything else. A non-empty value
		// here would mean the Unix path started claiming a choice it does
		// not have.
		t.Errorf("pin = %q, want empty on a platform with nothing to pin", pin)
	}
}
