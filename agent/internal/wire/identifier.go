package wire

import (
	"errors"
	"fmt"
	"strings"
)

// MaxIdentifierLen bounds an id so a pathological value cannot build a
// subject the server refuses on length alone. Every id this protocol really
// uses is a UUID or a short slug, so 128 is generous.
const MaxIdentifierLen = 128

// ErrInvalidIdentifier is what every rejection wraps. Callers match it with
// errors.Is to turn a bad id into a refusal instead of an action.
var ErrInvalidIdentifier = errors.New("wire: invalid identifier")

// ValidIdentifier reports whether s is safe to interpolate into a NATS
// subject. Only [A-Za-z0-9._-] is accepted.
//
// This is not defensive tidiness. Two characters change what a subject
// MEANS: "*" widens one token and ">" widens the rest of them. A session id
// of ">" makes the agent's own Subscribe illegal, the server answers
// -ERR 'Invalid Subject', and nats.go does not recognise that error, so it
// closes the connection for good. The process stays up, keeps heartbeating
// into a dead socket, and never gets restarted: one message makes a machine
// permanently unmanageable. A session id of "*" is quieter and worse, since
// it subscribes a root PTY to every other session's keystrokes and eats
// their flow-control acks.
//
// Empty dot-separated segments are refused too, so an id can neither
// contribute an empty subject token nor read as "." or ".." where it is used
// to build a filename.
func ValidIdentifier(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty", ErrInvalidIdentifier)
	}
	if len(s) > MaxIdentifierLen {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrInvalidIdentifier, len(s), MaxIdentifierLen)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		case r == '*', r == '>':
			return fmt.Errorf("%w: %q contains the NATS wildcard %q", ErrInvalidIdentifier, s, string(r))
		case r == ' ', r == '\t', r == '\r', r == '\n', r == '\v', r == '\f':
			return fmt.Errorf("%w: %q contains whitespace", ErrInvalidIdentifier, s)
		default:
			return fmt.Errorf("%w: %q contains %q", ErrInvalidIdentifier, s, string(r))
		}
	}
	for _, tok := range strings.Split(s, ".") {
		if tok == "" {
			return fmt.Errorf("%w: %q has an empty dot-separated segment", ErrInvalidIdentifier, s)
		}
	}
	return nil
}

// CheckIdentifier is ValidIdentifier with the field name in the message, so
// a log line or a refusal reply says which id was wrong.
func CheckIdentifier(field, s string) error {
	if err := ValidIdentifier(s); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}
