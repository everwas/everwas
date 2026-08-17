package inventory

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Login is one interactive session on the machine.
//
// Answers "who is on this box, and where from": the question that starts most
// "why did this change" conversations. Stored bitemporally, it also answers the
// version of that question asked after the fact, which is the one that matters
// during an incident: who was logged in at 03:00 last Tuesday.
type Login struct {
	User string `json:"user"`
	// Terminal is the seat, in whatever the platform calls it: tty1 or pts/3
	// on Unix, "Console" or "RDP-Tcp#2" on Windows.
	Terminal string `json:"terminal,omitempty"`
	// Host is where they came from, empty for a local session.
	Host string `json:"host,omitempty"`
	// Kind normalises Terminal and Host into console or remote, so a fleet
	// query for "who is on a physical console" does not have to know the
	// naming conventions of three operating systems.
	Kind string `json:"kind"`
	// State is the session state where the platform has one. Windows keeps a
	// disconnected RDP session alive with its programs running, and that is a
	// materially different thing from being logged out; Unix has no equivalent
	// so this is empty there.
	State string `json:"state,omitempty"`
	Since string `json:"since,omitempty"`
}

const (
	kindConsole = "console"
	kindRemote  = "remote"
)

type loginSnapshot struct {
	Logins []Login `json:"logins"`
}

// collectLogins lists interactive sessions.
//
// An unreadable source is reported as an error, NOT as an empty list. The
// difference is the whole point: no rows means "nobody is logged in", which is
// a claim, and on a box with no utmp it would be a false one. Returning the
// error means this kind is not published, so the server keeps believing
// whatever it last knew instead of being told the machine is empty.
func collectLogins(ctx context.Context) (any, error) {
	logins, err := currentLogins(ctx)
	if err != nil {
		return nil, err
	}
	// Sorted because the snapshot is hashed for change detection and neither
	// utmp order nor the WTS enumeration order is promised to be stable.
	sort.Slice(logins, func(i, j int) bool {
		if logins[i].User != logins[j].User {
			return logins[i].User < logins[j].User
		}
		return logins[i].Terminal < logins[j].Terminal
	})
	return loginSnapshot{Logins: logins}, nil
}

// classify decides console or remote from the terminal and origin host.
//
// Host is the reliable signal and is checked first: an SSH session is remote
// whatever its pty is called. Terminal names are the fallback, and they are
// only a convention.
func classify(terminal, host string) string {
	if h := strings.TrimSpace(host); h != "" && h != ":0" && h != "localhost" {
		return kindRemote
	}
	t := strings.ToLower(terminal)
	switch {
	case strings.HasPrefix(t, "rdp-"), strings.HasPrefix(t, "pts/"), strings.HasPrefix(t, "ssh"):
		// A pts with no host is something like tmux or a terminal emulator on
		// a local desktop, but it is still not a physical console.
		return kindRemote
	default:
		return kindConsole
	}
}

// sinceString formats a login time, or empty if we do not have one.
//
// Empty rather than the zero time: "logged in at midnight 1970" is worse than
// admitting we could not read it, because it silently becomes the oldest
// session on every sorted list.
func sinceString(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
