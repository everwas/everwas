package enroll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExchange(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != enrollPath {
			t.Errorf("got %s %s, want POST %s", r.Method, r.URL.Path, enrollPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(response{
			AgentID:     "0198f6f2-0000-7000-8000-000000000001",
			AgentSecret: "issued-secret",
			NATSURL:     "wss://rmm.example.com/nats",
		})
	}))
	defer srv.Close()

	cfg, err := exchange(srv.URL+"/", "tok-123", "1.2.3") // trailing slash must not double up
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	if got.Token != "tok-123" {
		t.Errorf("token = %q, want tok-123", got.Token)
	}
	if got.AgentVersion != "1.2.3" {
		t.Errorf("agent_version = %q, want 1.2.3", got.AgentVersion)
	}
	if got.Hostname == "" {
		t.Error("hostname empty")
	}
	switch got.OSFamily {
	case "linux", "macos", "windows":
	default:
		t.Errorf("os_family = %q, want linux|macos|windows", got.OSFamily)
	}
	if got.Arch == "amd64" || got.Arch == "arm64" {
		t.Errorf("arch = %q, want mapped value (x86_64/aarch64)", got.Arch)
	}

	if cfg.AgentID != "0198f6f2-0000-7000-8000-000000000001" ||
		cfg.AgentSecret != "issued-secret" ||
		cfg.NATSURL != "wss://rmm.example.com/nats" ||
		cfg.ServerURL != srv.URL+"/" {
		t.Errorf("config mismatch: %+v", cfg)
	}
}

func TestExchangeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"invalid token"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := exchange(srv.URL, "bad", "dev"); err == nil {
		t.Error("want error on 403, got nil")
	}
}

func TestExchangeMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(response{AgentID: "only-id"})
	}))
	defer srv.Close()

	if _, err := exchange(srv.URL, "tok", "dev"); err == nil {
		t.Error("want error on incomplete response, got nil")
	}
}
