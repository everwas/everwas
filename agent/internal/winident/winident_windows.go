package winident

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// detect asks Windows directly. Through PowerShell rather than WMI over COM,
// for the same reason the posture checks do: COM needs a locked OS thread, it
// is stateful, and it hangs. This runs on a timer on somebody's laptop.
func detect(ctx context.Context) (Sources, error) {
	var s Sources

	domain, err := powershell(ctx, `(Get-CimInstance Win32_ComputerSystem).PartOfDomain`)
	if err != nil {
		return s, err
	}
	s.DomainJoined = strings.EqualFold(strings.TrimSpace(domain), "True")

	issuers, err := powershell(ctx,
		`(Get-ChildItem Cert:\LocalMachine\My -EA SilentlyContinue |`+
			` Where-Object { $_.HasPrivateKey -and `+
			`   ($_.EnhancedKeyUsageList.ObjectId -contains "1.3.6.1.5.5.7.3.2") } |`+
			` ForEach-Object { $_.Issuer }) -join ";"`)
	if err != nil {
		return s, err
	}
	for _, issuer := range strings.Split(strings.TrimSpace(issuers), ";") {
		if strings.TrimSpace(issuer) == "" {
			continue
		}
		s.EverwasCerts, s.OtherClientCerts = tally(issuer, s.EverwasCerts, s.OtherClientCerts)
	}

	// A missing policy key is the ordinary case, so a failure here is not worth
	// failing the whole detection over: the certificate counts above are the
	// stronger signal and we already have them.
	if gpo, err := powershell(ctx,
		`if (Test-Path "HKLM:\SOFTWARE\Policies\Microsoft\Windows\WiredL2\GP_Policy") { "yes" } else { "no" }`,
	); err == nil {
		s.GroupPolicyProfile = strings.EqualFold(strings.TrimSpace(gpo), "yes")
	}
	return s, nil
}

// issuedByUs matches the common name we set on the issuing CA in
// services/ca.py. Not a strong identity check and does not need to be: nothing
// here is an authorization decision, and a machine that manages to fool this
// gets its certificate renewed by us, which is the harmless direction.
func issuedByUs(issuer string) bool {
	return strings.Contains(issuer, "Device Issuing CA")
}

func tally(issuer string, ours, other int) (int, int) {
	if issuedByUs(issuer) {
		return ours + 1, other
	}
	return ours, other + 1
}

func powershell(ctx context.Context, expr string) (string, error) {
	path, err := exec.LookPath("powershell.exe")
	if err != nil {
		return "", errors.New("winident: powershell is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, path,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", expr)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}
