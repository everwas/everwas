// Package winident answers one question about a Windows machine: which systems
// are providing it an 802.1X identity.
//
// It exists as its own package because two callers need the answer for
// different reasons, and a second copy of the detection would drift from the
// first. The posture check REPORTS it, so an operator can see how much of a
// fleet is in each state. The certificate path DECIDES with it, so the agent
// does not quietly install a competing identity on a machine Active Directory
// is already provisioning.
//
// Detection only. It draws no conclusions about what should happen, because the
// two callers want different conclusions from the same facts.
package winident

import "context"

// Sources is what is provisioning this machine, as far as we can tell.
type Sources struct {
	// DomainJoined is context rather than evidence. A domain-joined machine
	// with no certificate and no policy is not being provisioned by AD, and a
	// workgroup machine can still have a certificate somebody installed by
	// hand.
	DomainJoined bool

	// EverwasCerts counts client-auth certificates in the machine store issued
	// by our device CA.
	EverwasCerts int

	// OtherClientCerts counts client-auth certificates from anyone else, which
	// on a domain-joined machine is almost always AD CS autoenrollment.
	OtherClientCerts int

	// GroupPolicyProfile reports a Group Policy 802.1X profile. It matters on
	// its own, without any certificate: a GPO machine profile takes precedence
	// over one added with netsh, so ours would be inert whether or not we hold
	// a certificate.
	GroupPolicyProfile bool
}

// ProvidedByUs reports whether this machine already has an identity from us.
//
// The load-bearing question for deferral. A machine we are already providing
// for must keep being provided for: stopping would let its certificate lapse,
// and an expired 802.1X certificate takes the machine off the network. So
// deferral only ever applies to machines we have not started on.
func (s Sources) ProvidedByUs() bool { return s.EverwasCerts > 0 }

// ProvidedByDirectory reports whether something else is already provisioning
// this machine, by certificate or by policy.
func (s Sources) ProvidedByDirectory() bool {
	return s.OtherClientCerts > 0 || s.GroupPolicyProfile
}

// Detect inspects the machine. On platforms other than Windows it reports
// nothing found, which is correct: there is no Active Directory machine store
// to compete with.
func Detect(ctx context.Context) (Sources, error) { return detect(ctx) }
