package winident

import "testing"

func TestWeNeverStopProvidingForAMachineWeAlreadyProvideFor(t *testing.T) {
	// The rule the whole file exists for. Deferring here would stop renewing a
	// certificate the machine is actively using; it would expire, and an
	// expired 802.1X certificate takes the machine off the network with no
	// remote way back. Detection can be wrong, and this is the direction in
	// which being wrong is survivable.
	for _, s := range []Sources{
		{EverwasCerts: 1},
		{EverwasCerts: 1, OtherClientCerts: 1},
		{EverwasCerts: 1, GroupPolicyProfile: true},
		{EverwasCerts: 1, OtherClientCerts: 3, GroupPolicyProfile: true, DomainJoined: true},
	} {
		if d := Decide(s, ModeAuto); d.Defer {
			t.Errorf("deferred on a machine we already provide for (%+v): %q", s, d.Reason)
		}
	}
}

func TestWeStandAsideForADirectoryThatGotThereFirst(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    Sources
		expect string
	}{
		{"an autoenrolled certificate", Sources{OtherClientCerts: 1, DomainJoined: true},
			"already has an 802.1X certificate issued by another authority"},
		{"a group policy profile with no certificate yet", Sources{GroupPolicyProfile: true, DomainJoined: true},
			"Group Policy 802.1X profile is already applied"},
		{"both", Sources{OtherClientCerts: 1, GroupPolicyProfile: true, DomainJoined: true},
			"certificate and a Group Policy profile"},
	} {
		d := Decide(tc.src, ModeAuto)
		if !d.Defer {
			t.Errorf("%s: did not defer", tc.name)
			continue
		}
		if !contains(d.Reason, tc.expect) {
			t.Errorf("%s: reason %q does not mention %q", tc.name, d.Reason, tc.expect)
		}
	}
}

func TestAMachineNobodyIsProvisioningIsOurs(t *testing.T) {
	// The common case, and the one that must not be accidentally deferred: a
	// workgroup machine, or a domain-joined one whose organisation has no PKI.
	for _, s := range []Sources{
		{},
		{DomainJoined: true},
	} {
		if d := Decide(s, ModeAuto); d.Defer {
			t.Errorf("deferred on a machine nobody is provisioning (%+v): %q", s, d.Reason)
		}
	}
}

func TestAlwaysIsForMigratingAwayFromTheDirectory(t *testing.T) {
	// An operator moving a fleet from AD CS to Everwas needs both present for a
	// while. Deliberately a decision somebody makes rather than a default.
	s := Sources{OtherClientCerts: 1, GroupPolicyProfile: true, DomainJoined: true}
	if d := Decide(s, ModeAlways); d.Defer {
		t.Errorf("force did not override deferral: %q", d.Reason)
	}
}

func TestADeferralAlwaysSaysWhy(t *testing.T) {
	// A machine silently not getting a certificate is indistinguishable from a
	// broken agent. Every deferral has to name the system that is doing the job
	// instead, or the log is just an absence.
	s := Sources{OtherClientCerts: 1}
	d := Decide(s, ModeAuto)
	if d.Defer && d.Reason == "" {
		t.Error("deferred without saying why")
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

func TestAnOperatorMayStopUsEvenThoughDetectionMayNot(t *testing.T) {
	// The asymmetry this design turns on. Detection deciding to stop is a
	// heuristic that could be wrong on a machine actively using our
	// certificate, so it is never allowed to. An operator deciding to stop is
	// somebody's choice, so it is honoured.
	s := Sources{EverwasCerts: 1}

	if d := Decide(s, ModeAuto); d.Defer {
		t.Error("detection stopped us on a machine we already provide for")
	}
	d := Decide(s, ModeNever)
	if !d.Defer {
		t.Fatal("an explicit never was ignored")
	}
	if !d.Warn {
		t.Error("stopping a machine that is USING our certificate was reported as routine")
	}
	if !contains(d.Reason, "expires") {
		t.Errorf("the reason does not name the consequence: %q", d.Reason)
	}
}

func TestNeverOnAMachineWeDoNotServeIsUneventful(t *testing.T) {
	// The ordinary case for a site that runs AD everywhere: nothing is lost, so
	// nothing should be warned about.
	d := Decide(Sources{OtherClientCerts: 1}, ModeNever)
	if !d.Defer {
		t.Fatal("never did not defer")
	}
	if d.Warn {
		t.Error("warned about a machine that was never ours to lose")
	}
}

func TestAMistypedModeIsRefusedRatherThanDefaulted(t *testing.T) {
	// A typo in "always" during a migration would otherwise leave a fleet
	// quietly deferring while an operator believed it was taking over, and the
	// only symptom would be machines that never got a certificate.
	for _, bad := range []string{"alwyas", "Always ", "yes", "true", "off"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) was accepted", bad)
		}
	}
	for _, good := range []string{"", "auto", "always", "never"} {
		if _, err := ParseMode(good); err != nil {
			t.Errorf("ParseMode(%q) was refused: %v", good, err)
		}
	}
}

func TestTheDefaultIsToDeferRatherThanTakeOver(t *testing.T) {
	// An operator who sets nothing gets the cautious behaviour, not the
	// aggressive one.
	m, err := ParseMode("")
	if err != nil || m != ModeAuto {
		t.Fatalf("empty mode = %q, %v; want auto", m, err)
	}
	if d := Decide(Sources{OtherClientCerts: 1, DomainJoined: true}, m); !d.Defer {
		t.Error("the default took over a machine Active Directory was provisioning")
	}
}
