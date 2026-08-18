#!/bin/sh
# Pre-remove for the everwas-agent deb and rpm.
#
# On an upgrade the package manager reinstalls immediately afterwards, so the
# service is left alone and postinstall restarts it. On a real removal it is
# stopped and disabled. The identity in /etc/everwas is deliberately kept:
# deleting it would orphan the agent record on the server, and a purge is the
# operator's call, not the package's.
set -eu

is_upgrade() {
    # deb passes "upgrade <version>", rpm passes "1" when an upgrade is
    # removing the old package.
    case "${1:-}" in
        upgrade | deconfigure) true ;;
        1) true ;;
        *) false ;;
    esac
}

if is_upgrade "$@"; then
    exit 0
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop everwas-agent || true
    systemctl disable everwas-agent || true
fi

exit 0
