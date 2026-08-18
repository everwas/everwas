package notify

import (
	"context"
	"os/exec"
	"time"
)

// local sends a message box to every session on the machine.
//
// msg.exe rather than a toast, because of session 0 isolation: the agent is a
// service running in session 0, and a service physically cannot draw UI in a
// user's session. The documented ways across that boundary are a helper
// process running inside the user session, which is a second binary to ship,
// install, keep updated and keep from being killed, or msg.exe, which is built
// into Windows and exists for exactly this.
//
// The trade is honest: msg.exe produces a plain message box rather than a
// modern toast, and it is absent from Home editions. A blunt dialog that
// actually appears beats an elegant notification that a service cannot send.
func local(ctx context.Context, title, body string) error {
	path, err := exec.LookPath("msg.exe")
	if err != nil {
		return ErrNoOneToTell
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// "*" is every session on this machine. /TIME:0 leaves it up until it is
	// dismissed rather than timing out, because this is a deadline to act on
	// and not a status update.
	cmd := exec.CommandContext(ctx, path, "*", "/TIME:0", title+": "+body)
	if err := cmd.Run(); err != nil {
		// msg.exe exits non-zero when there is no session to receive it, which
		// on an unattended machine is the ordinary state rather than a fault.
		return ErrNoOneToTell
	}
	return nil
}
