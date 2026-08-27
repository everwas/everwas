package posture

import (
	"context"
	"strings"
)

// Checks is every check that means anything on this platform. See the Linux
// list for why registration is an explicit slice rather than init() magic.
func Checks() []Check {
	return []Check{
		winBitLockerCheck{},
		winFirewallCheck{},
		winAntivirusCheck{},
		identitySourceCheck{},
	}
}

// powershell runs a PowerShell expression and returns its trimmed output.
//
// Every Windows check here goes through PowerShell rather than WMI via COM.
// The COM route is faster and is what the patch collector uses, and it is also
// the single riskiest component in this agent: it needs a locked OS thread, it
// is stateful, and it hangs. A posture check runs on a timer on somebody's
// laptop and is worth none of that. The cost is process startup, which the
// check timeout absorbs.
func powershell(ctx context.Context, expr string) (string, error) {
	return output(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", expr)
}

// winBitLockerCheck reports whether the system drive is encrypted.
//
// The system drive specifically. A machine with an encrypted data disk and a
// plaintext C: is not an encrypted machine, and reporting on "any volume" would
// call it one.
type winBitLockerCheck struct{}

func (winBitLockerCheck) Name() string { return "disk-encryption" }

func (winBitLockerCheck) Category() Category { return CategoryEncryption }

func (winBitLockerCheck) Run(ctx context.Context) Result {
	const name = "disk-encryption"

	out, err := powershell(ctx,
		`(Get-BitLockerVolume -MountPoint $env:SystemDrive).ProtectionStatus`)
	if err != nil {
		// Get-BitLockerVolume is absent on Home editions, where BitLocker is
		// not available at all. That is a machine we cannot assess, not one
		// that failed: the user cannot turn on a feature they do not have.
		return unknown(name,
			"BitLocker status could not be read; the cmdlet may be unavailable on this edition")
	}

	switch strings.TrimSpace(out) {
	case "On", "1":
		return pass(name, "the system drive is protected by BitLocker",
			map[string]string{"mechanism": "bitlocker"})
	case "Off", "0":
		return fail(name, "the system drive is not encrypted", nil)
	default:
		return unknown(name, "BitLocker reported an unrecognised protection status")
	}
}

// winFirewallCheck reports whether every network profile has the firewall on.
//
// Every profile, not any. A machine with Domain enabled and Public disabled is
// unprotected precisely where it matters, on the untrusted network it is
// carried to.
type winFirewallCheck struct{}

func (winFirewallCheck) Name() string { return "firewall" }

func (winFirewallCheck) Category() Category { return CategoryFirewall }

func (winFirewallCheck) Run(ctx context.Context) Result {
	const name = "firewall"

	out, err := powershell(ctx,
		`(Get-NetFirewallProfile | ForEach-Object { "$($_.Name)=$($_.Enabled)" }) -join ";"`)
	if err != nil || strings.TrimSpace(out) == "" {
		return unknown(name, "the firewall profiles could not be read")
	}

	evidence := map[string]string{}
	var disabled []string
	for _, part := range strings.Split(strings.TrimSpace(out), ";") {
		profile, state, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		evidence[strings.ToLower(profile)] = state
		if !strings.EqualFold(state, "True") {
			disabled = append(disabled, profile)
		}
	}
	if len(evidence) == 0 {
		return unknown(name, "no firewall profiles were reported")
	}
	if len(disabled) > 0 {
		return fail(name,
			"the firewall is disabled for the "+strings.Join(disabled, ", ")+" profile", evidence)
	}
	return pass(name, "the firewall is enabled for every network profile", evidence)
}

// winAntivirusCheck reports whether real-time antivirus protection is on.
//
// Reads Defender's own status rather than the Security Center registry,
// because a third-party product registers with Security Center and Defender
// stands down when it does. Asking Defender whether IT is running would then
// report "off" on a machine that is perfectly well protected by something else.
type winAntivirusCheck struct{}

func (winAntivirusCheck) Name() string { return "antivirus" }

func (winAntivirusCheck) Category() Category { return CategoryMalware }

func (winAntivirusCheck) Run(ctx context.Context) Result {
	const name = "antivirus"

	// Security Center is the authority on "is SOMETHING protecting this
	// machine", which is the question worth asking. productState's bit 12
	// (0x1000) is the enabled flag, a documented-by-observation encoding that
	// every tool in this space relies on.
	out, err := powershell(ctx,
		`(Get-CimInstance -Namespace root/SecurityCenter2 -ClassName AntiVirusProduct |`+
			` ForEach-Object { "$($_.displayName)=$($_.productState)" }) -join ";"`)
	if err != nil {
		return unknown(name, "the Security Center antivirus registration could not be read")
	}
	out = strings.TrimSpace(out)
	if out == "" {
		// Security Center answered and listed nothing. That IS a verdict: no
		// antivirus product is registered on this machine.
		return fail(name, "no antivirus product is registered with Windows Security Center", nil)
	}

	evidence := map[string]string{}
	enabled := false
	for _, part := range strings.Split(out, ";") {
		product, state, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		on := productStateEnabled(state)
		evidence[product] = state
		enabled = enabled || on
	}
	if enabled {
		return pass(name, "an antivirus product is registered and enabled", evidence)
	}
	return fail(name, "an antivirus product is registered but not enabled", evidence)
}

// productStateEnabled decodes Security Center's productState bitfield.
//
// The second byte carries the on/off state: 0x10 means enabled. The encoding is
// not formally documented by Microsoft, so an unparseable or unfamiliar value
// is treated as not-enabled ONLY when the rest of the field parsed; a value we
// cannot parse at all leaves the caller to report unknown rather than guessing.
func productStateEnabled(state string) bool {
	var n int
	for _, r := range strings.TrimSpace(state) {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	return (n>>8)&0x10 != 0
}
