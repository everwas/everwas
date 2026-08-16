#!/bin/sh
# Post-install for the openrmm-agent deb and rpm.
#
# The service is only enabled when the host already has an identity. Starting
# an unenrolled agent just produces a log line and an exit, and on a fleet
# rollout that noise hides the hosts that really are broken.
set -eu

STATE_FILE=/etc/openrmm/agent.json

is_upgrade() {
    # deb passes "configure <old-version>", rpm passes "2" on upgrade.
    case "${1:-}" in
        configure) [ -n "${2:-}" ] ;;
        2) true ;;
        *) false ;;
    esac
}

if ! command -v systemctl >/dev/null 2>&1; then
    echo "openrmm-agent: systemctl not found, skipping service setup" >&2
    exit 0
fi

systemctl daemon-reload || true

# Restart from OUTSIDE this transaction's cgroup when we can. If the agent is
# the thing being upgraded during one of its own patch jobs, a plain
# `systemctl restart` tears down the cgroup that contains the dpkg run calling
# this very script, which SIGKILLs the package manager mid-transaction. A
# transient timer unit is owned by systemd, not by us, so it survives.
restart_agent() {
    if command -v systemd-run >/dev/null 2>&1; then
        if systemd-run --collect --quiet --on-active=5 \
            --unit=openrmm-agent-restart \
            systemctl try-restart openrmm-agent >/dev/null 2>&1; then
            return 0
        fi
    fi
    systemctl try-restart openrmm-agent || true
}

if [ -s "$STATE_FILE" ]; then
    if is_upgrade "$@"; then
        restart_agent
    else
        systemctl enable --now openrmm-agent || true
    fi
    exit 0
fi

cat <<'EOF'
openrmm-agent is installed but not enrolled, so the service was not started.

Enroll, then start it:

  sudo openrmm-agent enroll --server https://rmm.example.com --token YOUR_TOKEN
  sudo systemctl enable --now openrmm-agent
EOF
exit 0
