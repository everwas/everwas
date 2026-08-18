// Package notify tells the person sitting at the machine something the server
// cannot tell them.
//
// It exists for one failure in particular. A device whose network certificate
// is heading for expiry is, by definition, a device whose path to the server is
// broken; that is why renewal keeps failing. So every server-side alarm is
// blind in exactly the case that matters, and the only party still reachable is
// the person in front of the screen, who is also the only one who can act:
// plug into ethernet, join the VPN, come into the office.
//
// Best effort by construction. There may be nobody logged in, the machine may
// be a headless server, the desktop may use something we cannot reach. A
// failure to notify is never a failure of the thing that wanted to notify, so
// every path here returns an error the caller is expected to log and continue
// past.
package notify

import (
	"context"
	"errors"
)

// ErrNoOneToTell means the notification could not be delivered to anybody:
// nobody is logged in, or the machine has no channel we know how to use.
//
// Distinguished from a real failure because it is the ordinary state of a
// server in a rack, and a caller that logged it as an error would be warning
// about the absence of a human on a machine that never has one.
var ErrNoOneToTell = errors.New("notify: nobody to notify on this machine")

// Local delivers a short message to whoever is using this machine.
//
// The title and body are read by a person, not parsed, so they should say what
// is wrong and what to do about it. They must not contain a path, a serial, or
// anything else that means nothing to the person reading it.
func Local(ctx context.Context, title, body string) error {
	return local(ctx, title, body)
}
