//go:build !linux && !windows

package notify

import "context"

// local has no implementation on this platform yet.
//
// macOS would use osascript's display notification, or better a real user
// agent registered with Notification Center, and is deferred deliberately
// rather than half-built: a notification path that silently fails is worse
// than one that admits it is missing, because the whole point is being heard
// when nothing else can reach the machine.
func local(context.Context, string, string) error { return ErrNoOneToTell }
