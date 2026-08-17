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
// Connect dials NATS.
//
// onClosed fires when the connection is unrecoverably closed. onAsyncError
// fires for every asynchronous protocol error, of which the one that matters
// is a permissions violation: it does not close the connection and does not
// reconnect, so without this callback the agent stays "connected" while being
// unable to receive on the denied subject, indefinitely and silently.
func Connect(
	cfg *config.Config,
	log *slog.Logger,
	onClosed func(),
	onAsyncError func(error),
) (*nats.Conn, error) {
	shortID := cfg.AgentID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	nc, err := nats.Connect(cfg.NATSURL,
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
		nats.ConnectHandler(func(nc *nats.Conn) {
			log.Info("nats connected", "url", nc.ConnectedUrl())
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", nc.ConnectedUrl())
		}),
		// Without an ErrorHandler, a permissions violation is COMPLETELY
		// silent. nats.go sets an internal error, does not close the
		// connection and does not reconnect, and surfaces it only here: a
		// denied subscribe returns nil from Subscribe, the server replies
		// -ERR on the wire, and no message ever arrives on that subject
		// again. IsConnected() stays true, heartbeats publish, the console
		// shows the agent online and healthy, and there is no log line
		// anywhere. This is the single loudest thing the agent can do about
		// the quietest way it can fail.
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			log.Error("nats async error", "err", err, "subject", subject)
			if onAsyncError != nil {
				onAsyncError(err)
			}
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			log.Error("nats connection closed for good, the agent can no longer be reached",
				"last_err", nc.LastError())
			if onClosed != nil {
				onClosed()
			}
		}),
	)
	if err != nil {
		return nil, err
	}

	// RetryOnFailedConnect means Connect returns a conn that is not yet
	// established, which is what lets the agent start while the server is
	// down. The cost is that nothing has told us so, and every JetStream
	// publish then fails with nats.go's "headers not supported by this
	// server" -- because the INFO that advertises header support has not
	// arrived. That error names the SERVER for a problem that is entirely
	// local, and it cost an hour of looking at the wrong machine.
	//
	// Say it plainly instead. The matching "nats connected" line above is
	// what confirms it later.
	if !nc.IsConnected() {
		log.Warn("nats not connected yet, retrying in the background",
			"url", cfg.NATSURL,
			"note", "until this connects, publishes fail with "+
				"\"headers not supported by this server\"; that is this "+
				"message, not a server problem")
	}
	return nc, nil
}
