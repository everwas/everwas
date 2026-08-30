package supplicant

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/everwas/everwas/agent/internal/secure"
)

// FileName is the generated profile inside the directory given to Write.
const FileName = "wpa_supplicant-everwas.conf"

// WindowsFileName is the same thing on Windows, and it is a different thing:
// `netsh lan add profile` takes an XML document and would reject a
// wpa_supplicant config with an error about the file rather than about the
// format.
const WindowsFileName = "everwas-8021x.xml"

// WindowsProfileName is what the profile is called once netsh has it.
const WindowsProfileName = "Everwas 802.1X"

// Write renders the profile for THIS platform and writes it, replacing any
// previous one.
//
// Returns the path written. It does NOT start or reload a supplicant: see the
// package comment for why applying is deliberately a separate, human decision.
//
// Written through a temporary file and renamed into place, because a
// supplicant reading this file while it is half-written gets a config that
// parses to something other than what either side intended, and the failure
// appears as an authentication problem rather than as a truncated file.
func Write(dir string, p Profile) (string, error) {
	name, content, err := renderFor(runtime.GOOS, p)
	if err != nil {
		return "", err
	}
	// The directory is created with the same protection as the rest of the
	// agent's state. The profile itself holds no secret, but it names the path
	// of the private key and the identity this machine authenticates as, and
	// neither is worth handing to every local user.
	if err := secure.MkdirAll(dir); err != nil {
		return "", err
	}

	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("supplicant: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("supplicant: install %s: %w", path, err)
	}
	return path, nil
}

// renderFor picks the form of profile the named platform can actually consume.
//
// The goos is a parameter rather than a build tag so that a Linux test can
// check what a Windows machine would get. That is not a stylistic preference:
// RenderWindows was written, tested and verified against real netsh, and was
// then unreachable from the command an operator runs, because Write had no
// branch at all. Testing the renderer without testing the choice of renderer is
// how that happens.
func renderFor(goos string, p Profile) (name, content string, err error) {
	if goos != "windows" {
		content, err = Render(p)
		return FileName, content, err
	}

	// Validated even though RenderWindows interpolates none of these: refusing
	// the same inputs on both platforms means an operator who tests a profile
	// on Linux and deploys on Windows does not discover a different set of
	// rules.
	if err := validate(p); err != nil {
		return "", "", err
	}
	if !p.Wired() {
		// A wireless 802.1X profile on Windows is a WLANProfile, a different
		// document with a different schema added by `netsh wlan`, not `netsh
		// lan`. Emitting the wired one for an SSID would produce a file netsh
		// accepts nowhere.
		return "", "", fmt.Errorf(
			"%w: wireless 802.1X on Windows needs a WLAN profile, which this does not generate yet",
			ErrInvalidProfile)
	}
	if strings.TrimSpace(p.ClientIssuerThumbprint) == "" {
		// Refused rather than rendered unpinned. RenderWindows treats an empty
		// list as "let Windows choose", which is a documented fallback there
		// and a silent regression here: the pin was added, verified against
		// real netsh, and then never reached from this function, which called
		// RenderWindows with nothing in it — the exact verified-but-unreachable
		// failure the comment above describes, repeated one level down. An
		// error names the missing piece; an unpinned profile authenticates
		// with whatever certificate Windows fancies until a decoy shows up.
		return "", "", fmt.Errorf(
			"%w: no issuer thumbprint to pin the client certificate with; on Windows the profile must name which certificate to present",
			ErrInvalidProfile)
	}
	content, err = RenderWindows(WindowsProfile{
		Name:                    WindowsProfileName,
		ClientIssuerThumbprints: []string{p.ClientIssuerThumbprint},
		// ServerCAThumbprints stays empty on purpose: that field pins which CA
		// the RADIUS SERVER may chain to, a per-site trust decision nobody has
		// made yet, and inventing one here would be guessing with someone
		// else's network. Empty means the machine's trusted roots, as before.
	})
	return WindowsFileName, content, err
}
