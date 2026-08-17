package agentcore

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// LinkHealth records asynchronous NATS protocol errors so something can act on
// them.
//
// The error that matters is a permissions violation. nats.go treats it as
// transient: it does not close the connection and does not reconnect, so a
// denied subscribe returns nil, the subject goes quiet forever, and
// IsConnected() keeps saying true. Every other health signal the agent has
// (heartbeats publishing, the process being alive, the connection being up)
// stays green while the agent cannot be told to do anything.
//
// This exists mainly so the post-update probe can refuse to confirm a build in
// that state. Confirming it would clear the probation marker and delete the
// rollback, leaving a permanently unmanageable host with no way back.
type LinkHealth struct {
	mu sync.Mutex
	// violation is kept SEPARATELY from the last error and is never
	// overwritten by a subsequent benign one. A slow-consumer warning or a
	// reconnect notice arriving after a denied subscribe must not erase the
	// denial: the grant is server-side and the agent is still deaf.
	violation error
	at        time.Time
	count     int
}

func NewLinkHealth() *LinkHealth { return &LinkHealth{} }

// RecordAsyncError is the nats.ErrorHandler sink.
func (h *LinkHealth) RecordAsyncError(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	if isPermissionViolation(err) {
		h.violation, h.at = err, time.Now()
	}
}

func isPermissionViolation(err error) bool {
	// nats.go surfaces this as ErrPermissionViolation for subscribes, but a
	// denied PUBLISH arrives only as text on some server versions, so match
	// both rather than trusting one shape.
	return errors.Is(err, errPermissionViolation) ||
		strings.Contains(strings.ToLower(err.Error()), "permissions violation")
}

// PermissionViolation reports the most recent permissions error, or nil.
//
// Permissions errors are treated as sticky rather than cleared on reconnect,
// because the grant is server-side configuration: it does not fix itself, and
// an agent that saw one is one that has been silently unable to work since.
// Clearing it on reconnect would let the next probe pass while nothing had
// changed.
func (h *LinkHealth) PermissionViolation() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.violation
}

// errPermissionViolation mirrors nats.ErrPermissionViolation without importing
// nats into this package, which otherwise has no dependency on it.
var errPermissionViolation = errors.New("nats: permissions violation")
