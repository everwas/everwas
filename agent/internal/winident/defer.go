package winident

// Decision is whether the agent should provide this machine's 802.1X identity.
type Decision struct {
	// Defer means do not request, install or configure anything.
	Defer bool
	// Reason is written to a log an operator reads, so it says which system is
	// already doing the job rather than merely that we stopped.
	Reason string
}

// Decide reports whether to leave this machine's 802.1X identity alone.
//
// The rule that matters is the second one, and it is the reason this is a
// function rather than a condition inline at the call site.
//
// FIRST: if we are already this machine's identity source, never defer.
// Deferring there would stop renewing a certificate the machine is actively
// using, it would expire, and an expired 802.1X certificate takes the machine
// off the network. Detection can be wrong, and this is the direction in which
// being wrong is survivable: we would keep renewing a certificate nobody uses,
// which costs a little CA load and nothing else. Stopping is not survivable, so
// nothing here can decide to stop.
//
// SECOND: if something else is already providing an identity and we are not,
// defer. Installing a second client-auth certificate makes selection
// non-deterministic, and adding a netsh profile under a Group Policy machine
// profile does nothing at all, so taking over is not even a clean takeover.
//
// `force` exists for the migration case, where an operator genuinely does want
// to move a fleet from AD CS to Everwas and needs both present for a while. It
// is deliberately a decision somebody makes, not a default.
func Decide(s Sources, force bool) Decision {
	if s.ProvidedByUs() {
		// Includes the both-present case. Once we are one of the sources, we
		// keep being one; resolving the conflict is an operator's call, and the
		// posture check is what tells them it exists.
		return Decision{}
	}
	if force {
		return Decision{}
	}
	if s.ProvidedByDirectory() {
		return Decision{
			Defer:  true,
			Reason: directoryReason(s),
		}
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
