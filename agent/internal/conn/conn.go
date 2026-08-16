// Package conn owns the NATS connection: agent credentials, connection
// naming, and infinite reconnect with backoff.
package conn

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/config"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

// Connect dials cfg.NATSURL as user=agent_id pass=agent_secret. It retries
// forever, so it succeeds even while the server is down; the returned conn
// (re)connects in the background.
//
// onClosed fires when the connection is closed for good. That is not the
// same as a disconnect: reconnect handles disconnects, and nothing recovers
// a closed conn. It happens after a fatal protocol error (an illegal
// subscribe subject is one), and it leaves a process that is running,
// heartbeating into nothing, and unable to receive a single command ever
// again. The caller must treat it as fatal and exit, because a service
// manager restarting a deaf agent is the only thing that fixes it. Health is
// not "the process is up"; it is "the process can still be told what to do".
func Connect(cfg *config.Config, log *slog.Logger, onClosed func()) (*nats.Conn, error) {
	shortID := cfg.AgentID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return nats.Connect(cfg.NATSURL,
		nats.UserInfo(cfg.AgentID, cfg.AgentSecret),
		nats.Name("openrmm-agent-"+shortID),
		// The server grants only this agent's own inbox; the shared
		// default would be refused, leaving every request unanswered.
		nats.CustomInboxPrefix(wire.InboxPrefix(cfg.AgentID)),
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
		nats.ClosedHandler(func(nc *nats.Conn) {
			log.Error("nats connection closed for good, the agent can no longer be reached",
				"last_err", nc.LastError())
			if onClosed != nil {
				onClosed()
			}
		}),
	)
}
