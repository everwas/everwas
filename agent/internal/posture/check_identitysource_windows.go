package posture

import (
	"context"
	"strconv"
	"strings"
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
// So this reports the conflict rather than resolving it. The agent should not
// quietly take over a machine that Active Directory is already provisioning,
// and an operator cannot make that call without first knowing how many machines
// are in that state.
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

	domain, err := powershell(ctx, `(Get-CimInstance Win32_ComputerSystem).PartOfDomain`)
	if err != nil {
		return unknown(name, "could not determine whether this machine is domain-joined")
	}
	joined := strings.EqualFold(strings.TrimSpace(domain), "True")

	// Client-auth certificates in the machine store, grouped by issuer. Ours
	// are issued by the Everwas device CA; anything else on a domain-joined
	// machine is almost certainly the enterprise CA via autoenrollment.
	out, err := powershell(ctx,
		`(Get-ChildItem Cert:\LocalMachine\My -EA SilentlyContinue |`+
			` Where-Object { $_.HasPrivateKey -and `+
			`   ($_.EnhancedKeyUsageList.ObjectId -contains "1.3.6.1.5.5.7.3.2") } |`+
			` ForEach-Object { $_.Issuer }) -join ";"`)
	if err != nil {
		return unknown(name, "could not read the machine certificate store")
	}

	var ours, other int
	for _, issuer := range strings.Split(strings.TrimSpace(out), ";") {
		issuer = strings.TrimSpace(issuer)
		if issuer == "" {
			continue
		}
		// Matched on the issuing CA's common name, which we set ourselves in
		// services/ca.py. Not a strong identity check and does not need to be:
		// this is a report about which systems are provisioning the machine,
		// not an authorization decision.
		if strings.Contains(issuer, "Device Issuing CA") {
			ours++
		} else {
			other++
		}
	}

	// A Group Policy 802.1X profile takes precedence over anything netsh adds,
	// so its presence means the agent's profile would be inert.
	gpo, _ := powershell(ctx,
		`if (Test-Path "HKLM:\SOFTWARE\Policies\Microsoft\Windows\WiredL2\GP_Policy") { "yes" } else { "no" }`)
	gpoProfile := strings.EqualFold(strings.TrimSpace(gpo), "yes")

	evidence := map[string]string{
		"domain_joined":      boolText(joined),
		"everwas_certs":      strconv.Itoa(ours),
		"other_client_certs": strconv.Itoa(other),
		"group_policy_8021x": boolText(gpoProfile),
	}

	switch {
	case ours > 0 && (other > 0 || gpoProfile):
		// The case worth surfacing. Both systems are provisioning this machine
		// and the outcome depends on heuristics neither of them controls.
		return fail(name,
			"both Everwas and Active Directory are providing an 802.1X identity to this machine",
			evidence)
	case ours > 0:
		return pass(name, "Everwas provides this machine's 802.1X identity", evidence)
	case other > 0 || gpoProfile:
		// Not a failure. A domain-joined machine getting its identity from AD
		// is a perfectly good arrangement, and the agent should leave it alone.
		return pass(name, "Active Directory provides this machine's 802.1X identity", evidence)
	default:
		// No 802.1X identity from anyone. We cannot tell whether this machine
		// is supposed to have one, and guessing would make every workstation in
		// a fleet that does not use 802.1X report a failure.
		return notApplicable(name, "no 802.1X identity is configured on this machine")
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
