package supplicant

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

func winProfile() WindowsProfile {
	return WindowsProfile{Name: "Everwas 802.1X"}
}

func TestTheProfileIsWellFormedXML(t *testing.T) {
	// netsh rejects a malformed profile without saying which element it
	// disliked, so the cheapest possible check is worth having before the
	// expensive one on a real machine.
	out, err := RenderWindows(winProfile())
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := xml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("generated profile is not well-formed XML: %v", err)
	}
}

func TestItAuthenticatesAsTheMACHINE(t *testing.T) {
	// Our certificate is a device identity: the Common Name is the device
	// UUID and it is issued to the machine, not to whoever is signed in.
	// authMode "user" would look for a credential in a user store that does
	// not exist, so the machine would fail to authenticate whenever nobody was
	// logged in, which is most of the time for a server and all of the time at
	// the login screen.
	out, err := RenderWindows(winProfile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<authMode>machine</authMode>") {
		t.Error("profile does not authenticate as the machine")
	}
	for _, wrong := range []string{">user<", ">userOrMachine<", ">machineOrUser<"} {
		if strings.Contains(out, wrong) {
			t.Errorf("profile uses %s, which needs a signed-in user", wrong)
		}
	}
}

func TestItSelectsEapTLSNotSomethingElse(t *testing.T) {
	// 13 is EAP-TLS. It appears in two places and a mismatch between them is
	// the kind of thing that produces a profile Windows accepts and never
	// authenticates with.
	out, err := RenderWindows(winProfile())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, ">13</Type>"); n != 2 {
		t.Errorf("EAP type 13 appears %d times, want 2 (EapMethod and Eap)", n)
	}
	// 25 is PEAP, the method this is most likely to be confused with.
	if strings.Contains(out, ">25</Type>") {
		t.Error("profile requests PEAP somewhere")
	}
}

func TestNoUserPromptForServerValidation(t *testing.T) {
	// A machine authenticating at the login screen has nobody to click
	// "trust this server". A prompt nobody can answer is a machine that never
	// authenticates, and it fails in a way that looks like a certificate
	// problem rather than a dialog waiting behind the login screen.
	out, err := RenderWindows(winProfile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<DisableUserPromptForServerValidation>true<") {
		t.Error("the profile would wait for a user to approve the server")
	}
}

func TestThumbprintsAreNormalisedNotPastedThrough(t *testing.T) {
	// Windows prints thumbprints spaced in some tools and unspaced in others.
	// The spaced form produces a profile netsh ACCEPTS and that never matches
	// a server, which is the worst combination: it looks configured and
	// silently trusts nothing.
	p := winProfile()
	p.ServerCAThumbprints = []string{"A1 B2 C3 D4 E5 F6 07 18 29 3A 4B 5C 6D 7E 8F 90 A1 B2 C3 D4"}
	out, err := RenderWindows(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<TrustedRootCA>a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4</TrustedRootCA>") {
		t.Errorf("thumbprint was not normalised:\n%s", out)
	}
}

func TestAThumbprintThatIsNotOneIsRefused(t *testing.T) {
	for _, bad := range []string{
		"not-a-thumbprint",
		"a1b2c3", // too short
		"a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4ff", // too long
		"g1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4",   // non-hex
	} {
		p := winProfile()
		p.ServerCAThumbprints = []string{bad}
		if _, err := RenderWindows(p); !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("%q was accepted as a thumbprint", bad)
		}
	}
}

func TestServerNamesAreEscapedNotInjected(t *testing.T) {
	// This one is XML, so the injection shape differs from wpa_supplicant's,
	// but the principle is the same: a value that closes its element and opens
	// another rewrites the profile rather than breaking it.
	p := winProfile()
	p.ServerNames = `radius</ServerNames><ServerNames>evil`
	out, err := RenderWindows(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "<ServerNames>") != 1 {
		t.Errorf("a crafted server name injected an extra element:\n%s", out)
	}
	var v any
	if err := xml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("escaping produced malformed XML: %v", err)
	}
}

func TestAProfileWithNoNameIsRefused(t *testing.T) {
	// netsh addresses profiles by name; an unnamed one cannot be removed or
	// replaced afterwards.
	if _, err := RenderWindows(WindowsProfile{}); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("err = %v, want ErrInvalidProfile", err)
	}
}

func TestItDoesNotEnforce8021X(t *testing.T) {
	// Enforcing means a machine that cannot authenticate has NO network at
	// all, including no route to the server that would fix it. That is the
	// opposite of the remediation posture in ADR-0004, where a failure is
	// supposed to land the device somewhere it can still be repaired.
	out, err := RenderWindows(winProfile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<OneXEnforced>false</OneXEnforced>") {
		t.Error("the profile enforces 802.1X, so a failed machine would have no path back")
	}
	if !strings.Contains(out, "<OneXEnabled>true</OneXEnabled>") {
		t.Error("802.1X is not enabled at all")
	}
}

func TestPinningTheIssuerTurnsOffWindowsOwnChoice(t *testing.T) {
	// SimpleCertSelection is only safe while the machine holds exactly one
	// client-auth certificate. A domain-joined machine with AD CS
	// autoenrollment has a second one in the same store, and Windows then picks
	// by its own heuristics with no say from us. Presenting the AD certificate
	// to a RADIUS server that trusts only our CA fails as an authentication
	// rejection with nothing pointing at the cause.
	p := winProfile()
	p.ClientIssuerThumbprints = []string{"A1 B2 C3 D4 E5 F6 07 18 29 3A 4B 5C 6D 7E 8F 90 A1 B2 C3 D4"}
	out, err := RenderWindows(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<SimpleCertSelection>false</SimpleCertSelection>") {
		t.Error("issuer is pinned but Windows was still left to choose")
	}
	if !strings.Contains(out, `<CAHashList Enabled="true">`) {
		t.Error("no client certificate filter was emitted")
	}
	if !strings.Contains(out, "<IssuerHash>a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4</IssuerHash>") {
		t.Errorf("issuer thumbprint missing or not normalised:\n%s", out)
	}
}

func TestWithNoIssuerToPinItFallsBackAndSaysSo(t *testing.T) {
	// Not a silent fallback: the profile still works, and the only reason to be
	// here is that we could not determine our own issuer, which should not
	// happen because the agent holds the chain.
	out, err := RenderWindows(winProfile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<SimpleCertSelection>true</SimpleCertSelection>") {
		t.Error("without a pinned issuer, Windows has to be allowed to choose")
	}
	if strings.Contains(out, "CAHashList") {
		t.Error("emitted an empty certificate filter, which would match nothing")
	}
}

func TestABadClientIssuerThumbprintIsRefused(t *testing.T) {
	p := winProfile()
	p.ClientIssuerThumbprints = []string{"nonsense"}
	if _, err := RenderWindows(p); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("err = %v, want ErrInvalidProfile", err)
	}
}

func TestTheTwoThumbprintListsAreNotConfusedForEachOther(t *testing.T) {
	// One decides what we SEND, the other decides who we ACCEPT. Swapping them
	// produces a profile that presents nothing usable and trusts the wrong CA,
	// so they must land in different elements.
	p := winProfile()
	p.ClientIssuerThumbprints = []string{"a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4"}
	p.ServerCAThumbprints = []string{"ffeeddccbbaa99887766554433221100ffeeddcc"}
	out, err := RenderWindows(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<IssuerHash>a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4</IssuerHash>") {
		t.Error("the client issuer did not reach IssuerHash")
	}
	if !strings.Contains(out, "<TrustedRootCA>ffeeddccbbaa99887766554433221100ffeeddcc</TrustedRootCA>") {
		t.Error("the server CA did not reach TrustedRootCA")
	}
	if strings.Contains(out, "<TrustedRootCA>a1b2c3d4") || strings.Contains(out, "<IssuerHash>ffeeddcc") {
		t.Error("the two thumbprint lists were crossed")
	}
}
