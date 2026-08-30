package config

import "testing"

func TestALocalOverrideBeatsTheServersPolicy(t *testing.T) {
	// The escape hatch. The reason to set one is that something is wrong with
	// this particular machine, and changing the whole fleet to fix it would be
	// the wrong move.
	c := &Config{NetworkIdentity: "always", NetworkIdentityPolicy: "never"}
	if got := c.EffectiveNetworkIdentity(); got != "always" {
		t.Errorf("effective = %q, want the local override", got)
	}
}

func TestTheServersPolicyAppliesWhenNobodyOverrodeIt(t *testing.T) {
	c := &Config{NetworkIdentityPolicy: "never"}
	if got := c.EffectiveNetworkIdentity(); got != "never" {
		t.Errorf("effective = %q, want the server policy", got)
	}
}

func TestRemovingAnOverrideFallsBackToTheFleetPolicyNotToNothing(t *testing.T) {
	// The reason the two are separate fields. With one field, a renewal would
	// have overwritten the operator's value and the fleet policy underneath it
	// would be gone, so clearing the override would leave the machine on the
	// default rather than on what the organization actually decided.
	c := &Config{NetworkIdentity: "always", NetworkIdentityPolicy: "never"}
	c.NetworkIdentity = ""
	if got := c.EffectiveNetworkIdentity(); got != "never" {
		t.Errorf("effective = %q, want the fleet policy to reappear", got)
	}
}

func TestNothingSetAnywhereIsEmptyRatherThanAGuess(t *testing.T) {
	// Empty parses to auto downstream. Returning "auto" here instead would
	// erase the difference between an organization choosing the cautious
	// behaviour and nobody having decided at all.
	if got := (&Config{}).EffectiveNetworkIdentity(); got != "" {
		t.Errorf("effective = %q, want empty", got)
	}
}
