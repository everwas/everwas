package agentcore

import (
	"errors"
	"testing"
)

// A permissions violation is the quietest way this agent can fail: nats.go
// does not close the connection and does not reconnect, so every other health
// signal stays green while the agent cannot receive on the denied subject.
func TestAPermissionsViolationIsRemembered(t *testing.T) {
	h := NewLinkHealth()
	if err := h.PermissionViolation(); err != nil {
		t.Fatalf("a fresh link reported a violation: %v", err)
	}

	h.RecordAsyncError(errors.New(`nats: Permissions Violation for Subscription to "cmd.abc.>"`))
	if err := h.PermissionViolation(); err == nil {
		t.Fatal("a denied subscription was not remembered, so the post-update probe would " +
			"confirm a build that cannot receive a single job and delete its rollback")
	}
}

func TestOrdinaryAsyncErrorsAreNotTreatedAsPermissionViolations(t *testing.T) {
	h := NewLinkHealth()
	// Slow consumers and stale connections are transient and recover on their
	// own. Treating them as a permanent violation would block every update
	// from ever being confirmed on a busy host.
	h.RecordAsyncError(errors.New("nats: slow consumer, messages dropped"))
	if err := h.PermissionViolation(); err != nil {
		t.Errorf("a slow consumer was misread as a permissions violation: %v", err)
	}
}

func TestAViolationIsStickyAcrossReconnects(t *testing.T) {
	// The grant is server-side configuration. It does not fix itself, and an
	// agent that saw one has been silently unable to work since. Clearing it
	// on reconnect would let the next probe pass with nothing changed.
	h := NewLinkHealth()
	h.RecordAsyncError(errors.New("nats: Permissions Violation for Publish to \"agents.x.events\""))
	h.RecordAsyncError(errors.New("nats: connection reconnected"))
	if err := h.PermissionViolation(); err == nil {
		t.Error("a later benign error cleared a permissions violation")
	}
}

func TestNilErrorsAreIgnored(t *testing.T) {
	h := NewLinkHealth()
	h.RecordAsyncError(nil)
	if err := h.PermissionViolation(); err != nil {
		t.Errorf("recording nil produced a violation: %v", err)
	}
}
