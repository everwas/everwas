package posture

// Checks is every check that means anything on this platform.
//
// This list is the whole registration mechanism, deliberately. A package-level
// registry populated from init() would let a check register itself, at the cost
// of the set of checks becoming invisible: you would have to grep the package
// to answer "what does this agent assess". One explicit list per platform means
// adding a check is a file plus a line here, and reading the list tells you the
// answer.
func Checks() []Check {
	return []Check{
		diskEncryptionCheck{},
		firewallCheck{},
		antivirusCheck{},
	}
}
