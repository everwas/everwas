//go:build !windows

package inventory

import (
	"context"
	"time"

	pshost "github.com/shirou/gopsutil/v4/host"
)

// currentLogins reads utmp (Linux) or utmpx (macOS) through gopsutil, which
// already filters to USER_PROCESS entries and so skips the runlevel, boot-time
// and pending-LOGIN records those files also carry.
func currentLogins(ctx context.Context) ([]Login, error) {
	users, err := pshost.UsersWithContext(ctx)
	if err != nil {
		return nil, err
	}

	logins := make([]Login, 0, len(users))
	for _, u := range users {
		if u.User == "" {
			continue
		}
		logins = append(logins, Login{
			User:     u.User,
			Terminal: u.Terminal,
			Host:     u.Host,
			Kind:     classify(u.Terminal, u.Host),
			Since:    sinceString(time.Unix(int64(u.Started), 0)),
		})
	}
	return logins, nil
}
