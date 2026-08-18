package posture

import "context"

// antivirusCheck on Linux.
//
// Reported as not applicable rather than failed, and that is a considered
// position rather than a shortcut. Linux servers overwhelmingly do not run
// resident antivirus, and treating their not running it as non-compliance would
// fail most of a normal fleet for doing the normal thing. If a site does
// mandate an agent (ClamAV, a vendor EDR), that is a site-specific check worth
// writing as its own file rather than pretending this one covers it.
type antivirusCheck struct{}

func (antivirusCheck) Name() string { return "antivirus" }

func (antivirusCheck) Run(context.Context) Result {
	return notApplicable("antivirus",
		"resident antivirus is not part of the baseline on Linux")
}
