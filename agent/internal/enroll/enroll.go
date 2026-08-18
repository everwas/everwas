// Package enroll performs the one-time HTTPS enrollment handshake and saves
// the issued identity to the state file.
package enroll

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/everwas/everwas/agent/internal/config"
	"github.com/everwas/everwas/agent/internal/sysinfo"
)

const enrollPath = "/api/v1/agents/enroll"

type request struct {
	Token        string `json:"token"`
	Hostname     string `json:"hostname"`
	OSFamily     string `json:"os_family"`
	OSVersion    string `json:"os_version"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
}

type response struct {
	AgentID     string `json:"agent_id"`
	AgentSecret string `json:"agent_secret"`
	NATSURL     string `json:"nats_url"`
}

// Enroll exchanges a one-time token for an agent identity and persists it.
// agentVersion is cli.Version, passed in to avoid an import cycle.
func Enroll(serverURL, token, agentVersion string) error {
	cfg, err := exchange(serverURL, token, agentVersion)
	if err != nil {
		return err
	}
	return cfg.Save()
}

// exchange does the HTTP handshake without touching disk.
func exchange(serverURL, token, agentVersion string) (*config.Config, error) {
	hostname, _ := os.Hostname()
	body, err := json.Marshal(request{
		Token:        token,
		Hostname:     hostname,
		OSFamily:     sysinfo.OSFamily(),
		OSVersion:    sysinfo.OSVersion(),
		Arch:         sysinfo.Arch(),
		AgentVersion: agentVersion,
	})
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(serverURL, "/") + enrollPath
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("enroll request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("enroll rejected: %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}

	var r response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("parse enroll response: %w", err)
	}
	if r.AgentID == "" || r.AgentSecret == "" || r.NATSURL == "" {
		return nil, fmt.Errorf("enroll response missing fields (agent_id=%q nats_url=%q)", r.AgentID, r.NATSURL)
	}
	return &config.Config{
		ServerURL:   serverURL,
		AgentID:     r.AgentID,
		AgentSecret: r.AgentSecret,
		NATSURL:     r.NATSURL,
	}, nil
}
