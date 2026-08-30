package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/everwas/everwas/agent/internal/config"
	"github.com/everwas/everwas/agent/internal/netcert"
	"github.com/everwas/everwas/agent/internal/supplicant"
)

// CmdSupplicantProfile writes an 802.1X client profile for this device.
//
// A subcommand rather than something the agent does on its own, and that is
// the point rather than an omission. Generating the profile is safe; APPLYING
// one is how a fleet goes offline in a single push, because a machine that
// starts authenticating on a network not expecting it stops being on that
// network. So this produces a file and stops, and starting a supplicant
// against it stays an explicit decision made once per site after the profile
// has been tested against that site's own switches.
func CmdSupplicantProfile(args []string) int {
	fs := flag.NewFlagSet("supplicant-profile", flag.ContinueOnError)
	ssid := fs.String("ssid", "", "wireless network name; omit for wired 802.1X")
	out := fs.String("out", "", "directory to write the profile into (default: the agent state dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "supplicant-profile: %v\n", err)
		return 1
	}
	if !cfg.Enrolled() {
		fmt.Fprintln(os.Stderr, "supplicant-profile: not enrolled, so there is no identity to present")
		return 1
	}

	stateDir, err := config.Dir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "supplicant-profile: %v\n", err)
		return 1
	}
	certDir := netcertDir(stateDir)

	// Refuse to write a profile for a certificate that is not there. The
	// profile would be syntactically fine and would fail the handshake with an
	// error about the certificate, which sends whoever reads it looking at the
	// CA rather than at the machine that never obtained one.
	//
	// "There" is not the same place on the two platforms, and Presence knows
	// which one this build's supplicant reads: PEM files for wpa_supplicant,
	// LocalMachine\My for Windows, where the netcert flow writes no files at
	// all. Checking the directory here refused every Windows device that was
	// actually holding a working certificate in the store. Presence also
	// hands back the issuer pin the Windows profile needs, from the same
	// store, so the two cannot drift apart.
	issuerPin, err := netcert.Presence(certDir, cfg.AgentID)
	if err != nil {
		if errors.Is(err, netcert.ErrNoCertificate) {
			fmt.Fprintf(os.Stderr,
				"supplicant-profile: this device holds no network certificate (%v)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "supplicant-profile: %v\n", err)
		}
		return 1
	}

	dir := *out
	if dir == "" {
		dir = stateDir
	}
	path, err := supplicant.Write(dir, supplicant.Profile{
		// The device id, which is also the certificate's Common Name, so a
		// RADIUS session and a device in the console are the same string.
		Identity: cfg.AgentID,
		CertDir:  certDir,
		SSID:     *ssid,
		// "" on Unix, where there is nothing to pin. The Windows renderer
		// refuses an empty one rather than falling back to letting Windows
		// pick a certificate, so this cannot quietly go missing again.
		ClientIssuerThumbprint: issuerPin,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "supplicant-profile: %v\n", err)
		return 1
	}

	fmt.Printf("wrote %s\n", path)
	fmt.Println("Nothing is using it yet. To test it against a switch:")
	for _, line := range applySteps(runtime.GOOS, path) {
		fmt.Println("  " + line)
	}
	return 0
}

// applySteps is what an operator types next, which is not the same sequence on
// the two platforms and is not a difference worth making them discover.
//
// Verified on Windows 11: dot3svc, the Wired AutoConfig service, ships Stopped
// and set to Manual, and netsh refuses to add a LAN profile at all until it is
// running. An operator following the Linux instructions gets an error from
// netsh about the service, several steps away from the one thing they were
// told to do.
func applySteps(goos, path string) []string {
	if goos != "windows" {
		return []string{
			fmt.Sprintf("wpa_supplicant -c %s -i <interface> -D wired", path),
		}
	}
	return []string{
		"Start-Service dot3svc",
		fmt.Sprintf(`netsh lan add profile filename="%s" interface="Ethernet"`, path),
		"",
		"The certificate itself is already in LocalMachine\\My, put there by the",
		"agent when it was issued. Get-ChildItem Cert:\\LocalMachine\\My should",
		"show it with HasPrivateKey True; if it does not, the supplicant has no",
		"credential to present and the profile will not help.",
	}
}
