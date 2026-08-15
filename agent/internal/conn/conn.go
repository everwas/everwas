// Package conn owns the NATS connection: agent credentials, connection
// naming, and infinite reconnect with backoff.
package conn

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/openrmm/agent/internal/config"
)

// Connect dials cfg.NATSURL as user=agent_id pass=agent_secret. It retries
// forever, so it succeeds even while the server is down; the returned conn
// (re)connects in the background.
func Connect(cfg *config.Config, log *slog.Logger) (*nats.Conn, error) {
	shortID := cfg.AgentID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return nats.Connect(cfg.NATSURL,
		nats.UserInfo(cfg.AgentID, cfg.AgentSecret),
		nats.Name("openrmm-agent-"+shortID),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectWait(2*time.Second),
		nats.ReconnectJitter(500*time.Millisecond, 2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("nats disconnected", "err", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			log.Warn("nats connection closed")
		}),
	)
}
