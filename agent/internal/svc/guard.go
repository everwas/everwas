package svc

import (
	"os"
	"path/filepath"
	"runtime"
)

// The external rollback guard. It runs from the service manager BEFORE the
// agent, so it is the only rollback path that works when the new build cannot
// execute at all: wrong architecture, missing symbol, panic in package init,
// a config format it cannot parse. The agent's own crash counter needs the
// new binary to run in order to be incremented, which is exactly what those
// failures prevent.
//
// GuardScript is the source of truth. packaging/agent-guard.sh is a copy, and
// TestRenderedGuardMatchesPackagedGuard fails when the two drift.
const (
	// GuardName is the file name of the guard on disk.
	GuardName = "agent-guard.sh"

	// LinuxGuardPath is where the deb, the rpm and `everwas-agent install`
	// put the guard on Linux. The systemd unit references it from
	// ExecStartPre.
	LinuxGuardPath = "/usr/local/lib/everwas/" + GuardName

	// DarwinGuardPath is the macOS location. launchd has no ExecStartPre, so
	// there the guard is the daemon's ProgramArguments[0] and execs the agent
	// when it is done.
	DarwinGuardPath = "/Library/Everwas/Agent/" + GuardName
)

// GuardScript is the guard itself.
const GuardScript = `#!/bin/sh
# External rollback guard for the Everwas agent.
#
# The service manager runs this BEFORE the agent, which is the whole point:
# it covers the failures the agent's own crash counter cannot, because they
# happen before any of our Go code runs. A truncated download, the wrong
# architecture, a missing symbol, a panic in package init, a config format the
# new build cannot parse: all of those leave the in-process counter at zero
# forever, with a working binary sitting right next to the broken one.
#
#   agent-guard.sh check <target>            verify and restore, then return
#   agent-guard.sh exec  <target> [args...]  the same, then exec the agent
#
# It never exits non-zero. A guard that blocks startup is worse than the bug
# it guards against.
#
# This file is generated from internal/svc/guard.go. Change them together;
# TestRenderedGuardMatchesPackagedGuard fails if they drift.
set -u

MODE="${1:-check}"
TARGET="${2:-}"
if [ -z "$TARGET" ]; then
    exit 0
fi
shift 2 2>/dev/null || true

# This has to match config.Dir() exactly, or the guard looks for the probation
# in a directory the agent never writes to and silently stops counting.
STATE_DIR="${EVERWAS_STATE_DIR:-}"
if [ -z "$STATE_DIR" ]; then
    case "$(uname -s 2>/dev/null || echo Linux)" in
        Darwin) STATE_DIR="/Library/Application Support/Everwas" ;;
        *) STATE_DIR="/etc/everwas" ;;
    esac
fi
PROBATION="$STATE_DIR/update-probation"
STARTS="$STATE_DIR/update-starts"
DENIED="$STATE_DIR/update-denied"

# These match update.CrashWindow and update.CrashLimit. Change them together.
WINDOW=120
LIMIT=2
KEEP=16

log() { printf 'everwas-agent-guard: %s\n' "$*" >&2; }

probation_field() {
    [ -f "$PROBATION" ] || return 0
    sed -n "s/^$1=//p" "$PROBATION" 2>/dev/null | head -n 1
}

restore() {
    reason="$1"
    if [ ! -f "$BACKUP" ]; then
        log "$reason, but there is no backup at $BACKUP"
        return 1
    fi
    if ! mv -f "$BACKUP" "$TARGET"; then
        log "$reason, but restoring $BACKUP failed"
        return 1
    fi
    chmod 0755 "$TARGET" 2>/dev/null || true
    version="$(probation_field version)"
    if [ -n "$version" ]; then
        # The agent reads this at startup and refuses to install the version
        # again. Without it the server keeps sending the build this host just
        # restored itself from, and the whole fleet flaps on one bad release.
        printf '%s\n' "$version" >> "$DENIED" 2>/dev/null || true
    fi
    rm -f "$PROBATION" "$STARTS" 2>/dev/null || true
    log "restored the previous binary: $reason"
    return 0
}

BACKUP="$(probation_field backup)"
[ -n "$BACKUP" ] || BACKUP="$TARGET.old"

# 1. The binary is gone or cannot be executed. There is nothing the agent
#    could do about this, because it is not the thing running.
if [ ! -x "$TARGET" ]; then
    restore "$TARGET is missing or not executable" || true
fi

# 2. A build on probation that keeps coming back. Counting out here is the
#    only way to catch a build that dies before it can count for itself.
if [ -f "$PROBATION" ] && [ "$(probation_field finalizing)" != "true" ]; then
    now="$(date -u +%s 2>/dev/null || echo 0)"
    if [ "$now" != "0" ]; then
        printf '%s\n' "$now" >> "$STARTS" 2>/dev/null || true
        if [ -f "$STARTS" ]; then
            if tail -n "$KEEP" "$STARTS" > "$STARTS.tmp" 2>/dev/null; then
                mv -f "$STARTS.tmp" "$STARTS" 2>/dev/null || rm -f "$STARTS.tmp"
            fi
            recent="$(awk -v now="$now" -v w="$WINDOW" '
                /^[0-9]+$/ { if ($1 <= now && now - $1 <= w) n++ }
                END { print n + 0 }' "$STARTS" 2>/dev/null || echo 0)"
            if [ "$recent" -gt "$LIMIT" ]; then
                restore "$recent starts in ${WINDOW}s while version $(probation_field version) was on probation" || true
            fi
        fi
    fi
fi

case "$MODE" in
    exec) exec "$TARGET" "$@" ;;
    *) exit 0 ;;
esac
`

// DefaultGuardPath is where the guard belongs on this OS. Windows has no
// equivalent hook in the SCM, so it is empty there; see the recovery command
// configured in service_windows.go instead.
func DefaultGuardPath() string {
	switch runtime.GOOS {
	case "linux":
		return LinuxGuardPath
	case "darwin":
		return DarwinGuardPath
	default:
		return ""
	}
}

// GuardPath is the guard location, honouring the prefix override.
func GuardPath(prefix string) string {
	p := DefaultGuardPath()
	if p == "" {
		return ""
	}
	if prefix == "" {
		return p
	}
	return filepath.Join(prefix, p)
}

// writeGuard installs the guard script. It is written before the service
// definition that references it, so the unit never points at a file that is
// not there yet.
func writeGuard(prefix string) error {
	path := GuardPath(prefix)
	if path == "" {
		return nil
	}
	return writeFileAtomic(path, []byte(GuardScript), 0o755)
}

// removeGuard deletes the guard script, ignoring a guard that was never
// installed.
func removeGuard(prefix string) error {
	path := GuardPath(prefix)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
