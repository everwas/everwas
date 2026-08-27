package posture

import (
	"context"
	"strconv"

	"github.com/everwas/everwas/agent/internal/winident"
)

// identitySourceCheck reports which system owns this machine's 802.1X identity.
//
// In a Windows domain, Active Directory already does everything the agent does
// here: AD CS autoenrollment issues a machine certificate into the same store,
// and Group Policy pushes the 802.1X profile. Two systems then believe they own
// the same thing, and neither is told about the other.
//
// The consequences are quiet rather than loud. A Group Policy machine profile
// takes precedence over one added with netsh, so the agent can write a profile,
// report success, and change nothing. And with two client-auth certificates in
// the store, whichever one Windows picks may not be the one the RADIUS server
// trusts, which surfaces as an authentication rejection pointing at nothing.
//
// This REPORTS. The same detection decides whether the agent stands aside, in
// winident.Decide, and the two share one detector deliberately: a second copy
// of "is Active Directory providing this machine" would drift from the first,
// and then the machine an operator sees in the console and the machine the
// agent acts on would be different machines.
type identitySourceCheck struct{}

func (identitySourceCheck) Name() string { return "8021x-identity-source" }

// CategoryNetwork arrives with this check, which is the first one in it.
//
// Deliberately not defined in advance. A category that exists with no check in
// it looks covered: a site writes policy against it, no envelope ever carries
// it, absence never gates, and the policy sits there green and inert.
func (identitySourceCheck) Category() Category { return CategoryNetwork }

func (identitySourceCheck) Run(ctx context.Context) Result {
	const name = "8021x-identity-source"

	s, err := winident.Detect(ctx)
	if err != nil {
		return unknown(name, "could not determine which system provides this machine's 802.1X identity")
	}

	evidence := map[string]string{
		"domain_joined":      strconv.FormatBool(s.DomainJoined),
		"everwas_certs":      strconv.Itoa(s.EverwasCerts),
		"other_client_certs": strconv.Itoa(s.OtherClientCerts),
		"group_policy_8021x": strconv.FormatBool(s.GroupPolicyProfile),
	}

	switch {
	case s.ProvidedByUs() && s.ProvidedByDirectory():
		// The case worth surfacing. Both systems are provisioning this machine
		// and the outcome depends on heuristics neither of them controls.
		return fail(name,
			"both Everwas and Active Directory are providing an 802.1X identity to this machine",
			evidence)
	case s.ProvidedByUs():
		return pass(name, "Everwas provides this machine's 802.1X identity", evidence)
	case s.ProvidedByDirectory():
		// Not a failure. A domain-joined machine getting its identity from AD
		// is a perfectly good arrangement, and the agent stands aside for it.
		return pass(name, "Active Directory provides this machine's 802.1X identity", evidence)
	default:
		// No 802.1X identity from anyone. We cannot tell whether this machine
		// is supposed to have one, and guessing would make every workstation in
		// a fleet that does not use 802.1X report a failure.
		return notApplicable(name, "no 802.1X identity is configured on this machine")
	}
}
