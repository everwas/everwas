package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/everwas/everwas/agent/internal/config"
)

// RenewInterval is how often the agent asks for a fresh credential.
//
// Well under any server-side window, and unconditional rather than
// "if it looks old": the agent cannot tell whether an operator rotated while it
// was switched off, so the only safe assumption is that it might have been.
// Renewing is cheap and the failure it prevents is a site visit.
const RenewInterval = 12 * time.Hour

// renewPath mirrors enrollPath: a public endpoint where the credential being
// presented IS the authentication.
const renewPath = "/api/v1/agents/renew"

// ErrRenewRefused means the credential we presented is not accepted and never
// will be: the device was retired, or a revocation window closed while we were
// away. Distinguished from a transport failure so the caller stops retrying
// something that cannot succeed and says why instead.
var ErrRenewRefused = errors.New("renew: server refused the presented credential")

type renewRequest struct {
	AgentID     string `json:"agent_id"`
	AgentSecret string `json:"agent_secret"`
}

type renewResponse struct {
	AgentSecret string `json:"agent_secret"`
}

// Renew exchanges the credential this agent holds for a fresh one and saves it.
//
// PULL, and that is the entire fix. Rotation used to be pushed over NATS to a
// machine that might be off, with a deadline on the old secret and nothing
// retrying, so a laptop away for a long weekend came back holding a credential
// that had already expired and could not be given a new one. An agent that asks
// cannot miss the delivery, because it is the one asking.
//
// Ordering matters on failure. The new secret is written to disk BEFORE the old
// one is considered spent, and the server keeps the old one valid until this
// agent connects with the new one, so a crash between the reply and the save
// leaves a working credential either way. The dangerous ordering is the
// opposite: discard what works, then fail to persist its replacement.
func Renew(ctx context.Context, cfg *config.Config) error {
	if !cfg.Enrolled() {
		return fmt.Errorf("renew: not enrolled")
	}
	body, err := json.Marshal(renewRequest{
		AgentID:     cfg.AgentID,
		AgentSecret: cfg.AgentSecret,
	})
	if err != nil {
		return fmt.Errorf("renew: encode request: %w", err)
	}

	url := strings.TrimRight(cfg.ServerURL, "/") + renewPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("renew: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		// The credential we hold is not accepted. Retrying will not help: the
		// device was retired, or a revocation window closed while we were away.
		// Named separately so the caller can stop trying and say why.
		return ErrRenewRefused
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("renew: server returned %s", resp.Status)
	}

	var out renewResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("renew: decode response: %w", err)
	}
	if out.AgentSecret == "" {
		return fmt.Errorf("renew: server returned an empty secret")
	}

	cfg.AgentSecret = out.AgentSecret
	if err := cfg.Save(); err != nil {
		// The server has already rotated. We hold the new secret only in memory
		// and it is about to be lost, but the old one still works until we
		// connect with the new one, so the agent stays reachable and the next
		// attempt will get another.
		return fmt.Errorf("renew: save new credential: %w", err)
	}
	return nil
}
