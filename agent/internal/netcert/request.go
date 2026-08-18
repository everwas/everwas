package netcert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrNotConfigured means the server is not issuing certificates. Not an error
// to retry loudly: most deployments will never turn this on, so an agent that
// logged a warning every cycle would train operators to ignore its warnings.
var ErrNotConfigured = errors.New("netcert: server is not issuing certificates")

const certPath = "/api/v1/agents/certificate"

type request struct {
	AgentID     string `json:"agent_id"`
	AgentSecret string `json:"agent_secret"`
	CsrPEM      string `json:"csr_pem"`
}

type response struct {
	CertificatePEM string `json:"certificate_pem"`
	ChainPEM       string `json:"chain_pem"`
	Serial         string `json:"serial"`
}

// Request asks the server to sign a CSR and returns the certificate and chain.
//
// Authenticated by the agent credential, over the same HTTPS channel used for
// enrollment and renewal. That channel is the reason this works from a
// provisioning or quarantine VLAN: it is usually the one thing a device in that
// position is allowed to reach, which is exactly when it has no certificate.
func Request(
	ctx context.Context,
	serverURL, agentID, agentSecret string,
	csrPEM []byte,
) (certPEM, chainPEM string, err error) {
	body, err := json.Marshal(request{
		AgentID:     agentID,
		AgentSecret: agentSecret,
		CsrPEM:      string(csrPEM),
	})
	if err != nil {
		return "", "", fmt.Errorf("netcert: encode request: %w", err)
	}

	url := strings.TrimRight(serverURL, "/") + certPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("netcert: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("netcert: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		// The server has no CA, which is the normal state for a deployment
		// that does not use 802.1X. Distinguished so the caller can stay quiet
		// rather than warning forever about a feature nobody enabled.
		return "", "", ErrNotConfigured
	case http.StatusForbidden:
		return "", "", errors.New("netcert: server refused the certificate request")
	default:
		return "", "", fmt.Errorf("netcert: server returned %s", resp.Status)
	}

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("netcert: decode response: %w", err)
	}
	if out.CertificatePEM == "" || out.ChainPEM == "" {
		// A certificate with no chain cannot build a path, so accepting one
		// here would write material that fails authentication later, where the
		// cause is far less obvious.
		return "", "", errors.New("netcert: server returned an incomplete certificate")
	}
	return out.CertificatePEM, out.ChainPEM, nil
}

// Ensure obtains a certificate if the device has none or is due for renewal,
// and reports what it holds afterwards.
//
// Renewal generates a FRESH key rather than re-certifying the old one. A key
// that outlives many certificates turns "the certificate expired" into the only
// bound on how long a stolen key stays useful, which defeats the point of the
// ninety-day lifetime.
func Ensure(
	ctx context.Context,
	dir, serverURL, agentID, agentSecret string,
	now time.Time,
) (*Material, error) {
	existing, err := Load(dir)
	if err != nil && !errors.Is(err, ErrNoCertificate) {
		return nil, err
	}
	if existing != nil && !existing.DueForRenewal(now) {
		return existing, nil
	}

	// The new key is held in MEMORY until the certificate for it exists.
	//
	// Writing it first is the obvious shape and it destroys a working device on
	// a failed renewal: the key on disk no longer matches the certificate on
	// disk, so the machine drops off the network at its next reauthentication,
	// having been perfectly healthy until it tried to renew. Renewal must never
	// be able to leave the device worse than not renewing at all.
	key, keyPEM, err := newKeyPair()
	if err != nil {
		return nil, err
	}
	csr, err := buildCSRFor(key)
	if err != nil {
		return nil, err
	}
	certPEM, chainPEM, err := Request(ctx, serverURL, agentID, agentSecret, csr)
	if err != nil {
		// Nothing on disk was touched. The device still holds whatever it had,
		// which for a renewal is a certificate with weeks left on it.
		return nil, err
	}
	m, err := saveAll(dir, keyPEM, certPEM, chainPEM)
	if err != nil {
		return nil, err
	}
	m.Issued = true
	return m, nil
}
