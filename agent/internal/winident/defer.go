package winident

import "fmt"

// Mode is what an operator has decided about this machine's 802.1X identity.
//
// Three values rather than a boolean, because "off" and "override the
// detection" are different intentions and a site that uses Active Directory
// everywhere wants the first one. Expressing it as a boolean would make the
// common case (do not do this here at all) depend on detection happening to be
// right on every machine.
type Mode string

const (
	// ModeAuto defers to whatever is already provisioning the machine. The
	// default, and the right answer for a mixed estate.
	ModeAuto Mode = "auto"

	// ModeAlways provides an identity even where Active Directory already
	// does. For migrating a fleet from AD CS to Everwas, where both are
	// deliberately present for a while.
	ModeAlways Mode = "always"

	// ModeNever provides none, whatever is found. For a site whose Windows
	// estate is entirely AD-provisioned and does not want detection deciding.
	ModeNever Mode = "never"
)

// ParseMode reads the configured value. Empty is ModeAuto, and an unrecognised
// value is an ERROR rather than a silent fall back to the default: a typo in
// "always" during a migration would otherwise leave a fleet quietly deferring
// while an operator believed it was taking over.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeAlways:
		return ModeAlways, nil
	case ModeNever:
		return ModeNever, nil
	default:
		return ModeAuto, fmt.Errorf("winident: unknown network identity mode %q, want auto, always or never", s)
	}
}

// Decision is whether the agent should provide this machine's 802.1X identity.
type Decision struct {
	// Defer means do not request, install or configure anything.
	Defer bool

	// Reason is written to a log an operator reads, so it says which system is
	// already doing the job rather than merely that we stopped.
	Reason string

	// Warn marks a deferral that will cost this machine its network access,
	// rather than one that is simply the steady state. Only ModeNever on a
	// machine we already provide for produces it, and the caller logs it at a
	// level that gets read.
	Warn bool
}

// Decide reports whether to leave this machine's 802.1X identity alone.
//
// The rule that matters is the first one, and it is the reason this is a
// function rather than a condition inline at the call site.
//
// DETECTION MAY NEVER STOP US. If we are already this machine's identity
// source, no amount of finding Active Directory makes us defer. Deferring there
// would stop renewing a certificate the machine is actively using; it would
// expire, and an expired 802.1X certificate takes the machine off the network
// with no remote way back. Detection can be wrong, and this is the direction in
// which being wrong is survivable: we keep renewing a certificate nobody uses,
// which costs a little CA load and nothing else.
//
// An operator MAY stop us, because that is a decision somebody made rather than
// a heuristic that misfired. ModeNever on a machine we already serve is
// therefore honoured, and reported as a warning naming the consequence, because
// the person who set it may not have known this machine was one of ours.
func Decide(s Sources, m Mode) Decision {
	if m == ModeNever {
		if s.ProvidedByUs() {
			return Decision{
				Defer: true,
				Warn:  true,
				Reason: "configured not to provide a network identity, but this machine is " +
					"already using one from us: it will stop working when that certificate expires",
			}
		}
		return Decision{Defer: true, Reason: "configured not to provide a network identity"}
	}

	if s.ProvidedByUs() {
		// Includes the both-present case. Once we are one of the sources we
		// keep being one; resolving that conflict is an operator's call, and
		// the posture check is what tells them it exists.
		return Decision{}
	}
	if m == ModeAlways {
		return Decision{}
	}
	if s.ProvidedByDirectory() {
		return Decision{Defer: true, Reason: directoryReason(s)}
	}
	return Decision{}
}

func directoryReason(s Sources) string {
	switch {
	case s.OtherClientCerts > 0 && s.GroupPolicyProfile:
		return "this machine already has an 802.1X certificate and a Group Policy profile from elsewhere"
	case s.GroupPolicyProfile:
		// Worth naming separately: a Group Policy machine profile takes
		// precedence over anything netsh adds, so our profile would be written
		// and then ignored, which looks like success.
		return "a Group Policy 802.1X profile is already applied to this machine"
	default:
		return "this machine already has an 802.1X certificate issued by another authority"
	}
}
