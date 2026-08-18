package notify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// local tries the graphical sessions first and falls back to every terminal.
//
// Two audiences, and a machine can have either or both. A laptop user sees a
// desktop notification and nothing else; someone administering a server over
// SSH sees the wall broadcast and would never see a desktop toast. Trying the
// desktop first and only falling back keeps a workstation from being told
// twice, once in a popup and once splattered across their terminal.
func local(ctx context.Context, title, body string) error {
	msg := title + ": " + body

	notified := 0
	for _, uid := range graphicalSessions() {
		if err := notifySend(ctx, uid, title, body); err == nil {
			notified++
		}
	}
	if notified > 0 {
		return nil
	}
	return wall(ctx, msg)
}

// graphicalSessions returns the uids that currently have a user bus.
//
// /run/user/<uid>/bus is created by systemd for a logged-in user's session and
// removed when their last session ends, so its presence is a reasonable
// stand-in for "this person is here right now". Reading it needs no
// dependency on logind's API and no cgo.
func graphicalSessions() []int {
	entries, err := filepath.Glob("/run/user/*/bus")
	if err != nil {
		return nil
	}
	var uids []int
	for _, path := range entries {
		uid, err := strconv.Atoi(filepath.Base(filepath.Dir(path)))
		if err != nil {
			continue
		}
		// Skip system and greeter accounts. A notification delivered to the
		// login screen's own session is one nobody reads.
		if uid < 1000 {
			continue
		}
		uids = append(uids, uid)
	}
	return uids
}

// notifySend delivers a desktop notification as the given user.
//
// The agent runs as root, so this drops to the target uid and points at their
// session bus explicitly. Running notify-send as root without doing that sends
// it to root's own (usually nonexistent) bus, which fails in a way that looks
// like the desktop not supporting notifications.
func notifySend(ctx context.Context, uid int, title, body string) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path,
		"--urgency=critical",
		// Critical notifications on most desktops stay on screen until
		// dismissed, which is right here: this is not a status update, it is a
		// deadline the person needs to act on.
		"--app-name=OpenRMM",
		title, body,
	)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", uid),
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)},
	}
	return cmd.Run()
}

// wall broadcasts to every terminal, which is the only channel a headless
// server has.
func wall(ctx context.Context, msg string) error {
	path, err := exec.LookPath("wall")
	if err != nil {
		return ErrNoOneToTell
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "-n")
	cmd.Stdin = strings.NewReader(msg + "\n")
	if err := cmd.Run(); err != nil {
		// wall -n needs privileges to suppress its banner; without them it
		// still works, so retry plainly before giving up.
		plain := exec.CommandContext(ctx, path)
		plain.Stdin = strings.NewReader(msg + "\n")
		if err2 := plain.Run(); err2 != nil {
			return err
		}
	}
	return nil
}
