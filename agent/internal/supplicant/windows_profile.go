package supplicant

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// WindowsProfile is a wired 802.1X profile for the native Windows supplicant.
//
// Windows does not read PEM files from a directory the way wpa_supplicant
// does. Its supplicant takes its client credential from the machine
// certificate store and is configured by an XML profile that says WHICH
// certificate to use rather than where the bytes are. So this generates the
// profile; getting the certificate into the store is a separate step with its
// own failure modes, and deliberately not folded in here.
type WindowsProfile struct {
	// Name is what the profile is called in `netsh lan show profiles`.
	Name string

	// ServerCAThumbprints are the roots the RADIUS SERVER's certificate must
	// chain to, as SHA-1 thumbprints.
	//
	// Note whose CA this is: the server's, not ours. Our chain signs the
	// client certificate and is irrelevant to validating the far end. Getting
	// this backwards produces a profile that authenticates and trusts the
	// wrong thing, which is the failure a rogue RADIUS server exists to
	// exploit.
	//
	// Empty is permitted and is weaker: see the comment on server validation
	// in Render.
	ServerCAThumbprints []string

	// ServerNames optionally constrains the RADIUS server's subject. Empty
	// means any name that chains to a trusted root is accepted.
	ServerNames string
}

// eapTLSMethodType is EAP method 13. Named because a bare 13 appears four
// times in the XML below and a typo in one of them is not obvious.
const eapTLSMethodType = 13

// RenderWindows produces the LAN profile XML that `netsh lan add profile`
// consumes.
//
// Built as a string rather than through encoding/xml structs. The format nests
// five different namespaces, several elements are namespace-qualified in ways
// Go's marshaller expresses awkwardly, and Windows rejects the profile without
// saying which element it disliked. A literal that matches the documented
// shape is easier to check against Microsoft's schema than a struct graph that
// produces something almost like it.
func RenderWindows(p WindowsProfile) (string, error) {
	if strings.TrimSpace(p.Name) == "" {
		return "", fmt.Errorf("%w: the profile needs a name to be addressable by netsh", ErrInvalidProfile)
	}

	var b strings.Builder
	// UTF-8 rather than the US-ASCII that Microsoft's own examples declare.
	// netsh accepts both, and US-ASCII becomes a lie the moment a server name
	// carries a non-ASCII character, which produces a profile that parses
	// differently depending on who is reading it.
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<LANProfile xmlns="http://www.microsoft.com/networking/LAN/profile/v1">` + "\n")
	fmt.Fprintf(&b, "\t<MSM>\n\t\t<security>\n")

	// OneXEnforced false, OneXEnabled true: attempt 802.1X, but do not refuse
	// to use the link if authentication fails. Enforcing it would mean a
	// machine that cannot authenticate has no network at all, including no
	// route to the management server that would fix it, which is the opposite
	// of the remediation posture in ADR-0004.
	b.WriteString("\t\t\t<OneXEnforced>false</OneXEnforced>\n")
	b.WriteString("\t\t\t<OneXEnabled>true</OneXEnabled>\n")

	b.WriteString("\t\t\t<OneX xmlns=\"http://www.microsoft.com/networking/OneX/v1\">\n")
	// MACHINE authentication, not user or userOrMachine. Our certificate is a
	// device identity: its Common Name is the device UUID and it is issued to
	// the machine, not to whoever is logged in. "user" would look for a
	// credential in a user's store that does not exist, and the machine would
	// fail to authenticate whenever nobody was signed in, which is most of the
	// time for a server and all of the time at the login screen.
	b.WriteString("\t\t\t\t<authMode>machine</authMode>\n")
	b.WriteString("\t\t\t\t<EAPConfig>\n")
	b.WriteString("\t\t\t\t\t<EapHostConfig xmlns=\"http://www.microsoft.com/provisioning/EapHostConfig\">\n")
	b.WriteString("\t\t\t\t\t\t<EapMethod>\n")
	fmt.Fprintf(&b, "\t\t\t\t\t\t\t<Type xmlns=\"http://www.microsoft.com/provisioning/EapCommon\">%d</Type>\n", eapTLSMethodType)
	b.WriteString("\t\t\t\t\t\t\t<VendorId xmlns=\"http://www.microsoft.com/provisioning/EapCommon\">0</VendorId>\n")
	b.WriteString("\t\t\t\t\t\t\t<VendorType xmlns=\"http://www.microsoft.com/provisioning/EapCommon\">0</VendorType>\n")
	b.WriteString("\t\t\t\t\t\t\t<AuthorId xmlns=\"http://www.microsoft.com/provisioning/EapCommon\">0</AuthorId>\n")
	b.WriteString("\t\t\t\t\t\t</EapMethod>\n")
	b.WriteString("\t\t\t\t\t\t<Config xmlns=\"http://www.microsoft.com/provisioning/EapHostConfig\">\n")
	b.WriteString("\t\t\t\t\t\t\t<Eap xmlns=\"http://www.microsoft.com/provisioning/BaseEapConnectionPropertiesV1\">\n")
	fmt.Fprintf(&b, "\t\t\t\t\t\t\t\t<Type>%d</Type>\n", eapTLSMethodType)
	b.WriteString("\t\t\t\t\t\t\t\t<EapType xmlns=\"http://www.microsoft.com/provisioning/EapTlsConnectionPropertiesV1\">\n")
	b.WriteString("\t\t\t\t\t\t\t\t\t<CredentialsSource>\n")
	b.WriteString("\t\t\t\t\t\t\t\t\t\t<CertificateStore>\n")
	// SimpleCertSelection lets Windows pick the machine certificate whose EKU
	// and issuer fit. Ours is the only client-auth certificate the agent
	// installs, and it is marked clientAuth-critical with no serverAuth, which
	// is what makes an automatic choice safe rather than a guess.
	b.WriteString("\t\t\t\t\t\t\t\t\t\t\t<SimpleCertSelection>true</SimpleCertSelection>\n")
	b.WriteString("\t\t\t\t\t\t\t\t\t\t</CertificateStore>\n")
	b.WriteString("\t\t\t\t\t\t\t\t\t</CredentialsSource>\n")

	b.WriteString("\t\t\t\t\t\t\t\t\t<ServerValidation>\n")
	// No user to answer a prompt. A machine authenticating at the login screen
	// has nobody to click "yes, trust this server", and a prompt nobody can
	// answer is a machine that never authenticates.
	b.WriteString("\t\t\t\t\t\t\t\t\t\t<DisableUserPromptForServerValidation>true</DisableUserPromptForServerValidation>\n")
	fmt.Fprintf(&b, "\t\t\t\t\t\t\t\t\t\t<ServerNames>%s</ServerNames>\n", xmlEscape(p.ServerNames))
	for _, tp := range p.ServerCAThumbprints {
		clean := normaliseThumbprint(tp)
		if clean == "" {
			return "", fmt.Errorf("%w: %q is not a SHA-1 thumbprint", ErrInvalidProfile, tp)
		}
		fmt.Fprintf(&b, "\t\t\t\t\t\t\t\t\t\t<TrustedRootCA>%s</TrustedRootCA>\n", clean)
	}
	b.WriteString("\t\t\t\t\t\t\t\t\t</ServerValidation>\n")

	b.WriteString("\t\t\t\t\t\t\t\t\t<DifferentUsername>false</DifferentUsername>\n")
	b.WriteString("\t\t\t\t\t\t\t\t</EapType>\n")
	b.WriteString("\t\t\t\t\t\t\t</Eap>\n")
	b.WriteString("\t\t\t\t\t\t</Config>\n")
	b.WriteString("\t\t\t\t\t</EapHostConfig>\n")
	b.WriteString("\t\t\t\t</EAPConfig>\n")
	b.WriteString("\t\t\t</OneX>\n")
	b.WriteString("\t\t</security>\n\t</MSM>\n</LANProfile>\n")

	return b.String(), nil
}

// normaliseThumbprint strips the spaces certutil prints and lowercases the
// hex, returning "" if it is not 40 hex digits.
//
// Windows prints thumbprints as "a1 b2 c3 ..." in some tools and unspaced in
// others, and pasting the spaced form into the profile produces one that netsh
// accepts and that never matches a server, which is the worst combination.
func normaliseThumbprint(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == ':' || r == '-':
			continue
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			out.WriteRune(r)
		case r >= 'A' && r <= 'F':
			out.WriteRune(r + ('a' - 'A'))
		default:
			return ""
		}
	}
	if out.Len() != 40 {
		return ""
	}
	return out.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
