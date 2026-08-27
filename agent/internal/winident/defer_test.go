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
		if d := Decide(s, false); d.Defer {
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
		d := Decide(tc.src, false)
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
		if d := Decide(s, false); d.Defer {
			t.Errorf("deferred on a machine nobody is provisioning (%+v): %q", s, d.Reason)
		}
	}
}

func TestForceIsForMigratingAwayFromTheDirectory(t *testing.T) {
	// An operator moving a fleet from AD CS to Everwas needs both present for a
	// while. Deliberately a decision somebody makes rather than a default.
	s := Sources{OtherClientCerts: 1, GroupPolicyProfile: true, DomainJoined: true}
	if d := Decide(s, true); d.Defer {
		t.Errorf("force did not override deferral: %q", d.Reason)
	}
}

func TestADeferralAlwaysSaysWhy(t *testing.T) {
	// A machine silently not getting a certificate is indistinguishable from a
	// broken agent. Every deferral has to name the system that is doing the job
	// instead, or the log is just an absence.
	s := Sources{OtherClientCerts: 1}
	d := Decide(s, false)
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
