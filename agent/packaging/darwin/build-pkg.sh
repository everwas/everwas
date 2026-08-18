#!/bin/sh
# Build an unsigned macOS installer package for the Everwas agent.
#
#   ./packaging/darwin/build-pkg.sh dist/everwas-agent_darwin_universal/everwas-agent 1.2.3 dist
#
# Produces dist/Everwas-Agent-1.2.3.pkg, which drops the binary at
# /Library/Everwas/Agent/everwas-agent and the daemon plist at
# /Library/LaunchDaemons/systems.supported.everwas.agent.plist, then bootstraps the daemon.
#
# NOTARIZATION GAP: this package is unsigned and un-notarized. Gatekeeper will
# refuse it on a normal double click, so today it has to be installed from a
# terminal with `sudo installer -pkg ... -target /`, or through an MDM that
# pushes packages without user interaction. Closing the gap needs an Apple
# Developer ID Installer certificate plus:
#   productsign --sign "Developer ID Installer: ..." unsigned.pkg signed.pkg
#   xcrun notarytool submit signed.pkg --keychain-profile everwas --wait
#   xcrun stapler staple signed.pkg
# Those steps need credentials that do not belong in this repo, so they run in
# the release pipeline, not here.
set -eu

BINARY="${1:-}"
VERSION="${2:-0.0.0}"
OUTDIR="${3:-dist}"

IDENTIFIER="systems.supported.everwas.agent"
INSTALL_DIR="/Library/Everwas/Agent"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
STAGE=""

die() { printf '%s\n' "build-pkg: $*" >&2; exit 1; }

cleanup() {
    if [ -n "$STAGE" ] && [ -d "$STAGE" ]; then
        rm -rf -- "$STAGE"
    fi
}
trap cleanup EXIT INT TERM

[ -n "$BINARY" ] || die "usage: build-pkg.sh <binary> [version] [outdir]"
[ -f "$BINARY" ] || die "binary not found: $BINARY"
command -v pkgbuild >/dev/null 2>&1 || die "pkgbuild not found (macOS only)"
command -v productbuild >/dev/null 2>&1 || die "productbuild not found (macOS only)"

STAGE="$(mktemp -d)"
ROOT="$STAGE/root"
SCRIPTS="$STAGE/scripts"

mkdir -p "$ROOT$INSTALL_DIR" "$ROOT/Library/LaunchDaemons" "$ROOT/Library/Logs/Everwas" "$SCRIPTS" "$OUTDIR"

install -m 0755 "$BINARY" "$ROOT$INSTALL_DIR/everwas-agent"
# The daemon starts the guard, which execs the agent. Without this file the
# agent still runs (the plist tests for it first), but a build that cannot
# execute at all has no way back.
install -m 0755 "$SCRIPT_DIR/../agent-guard.sh" "$ROOT$INSTALL_DIR/agent-guard.sh"
install -m 0644 "$SCRIPT_DIR/systems.supported.everwas.agent.plist" "$ROOT/Library/LaunchDaemons/$IDENTIFIER.plist"

cat > "$SCRIPTS/postinstall" <<'POSTINSTALL'
#!/bin/sh
set -eu

PLIST=/Library/LaunchDaemons/systems.supported.everwas.agent.plist
STATE=/Library/Application\ Support/Everwas/agent.json

mkdir -p /Library/Logs/Everwas
chmod 0755 /Library/Logs/Everwas

# Only start an agent that has an identity. An unenrolled daemon under
# KeepAlive would respawn every five seconds and fill the log.
if [ -s "$STATE" ]; then
    launchctl bootout system/systems.supported.everwas.agent 2>/dev/null || true
    launchctl bootstrap system "$PLIST" 2>/dev/null || launchctl load -w "$PLIST"
else
    echo "everwas-agent installed but not enrolled; run:"
    echo "  sudo /Library/Everwas/Agent/everwas-agent install --server URL --token TOKEN"
fi
exit 0
POSTINSTALL
chmod 0755 "$SCRIPTS/postinstall"

COMPONENT="$STAGE/component.pkg"
pkgbuild \
    --root "$ROOT" \
    --scripts "$SCRIPTS" \
    --identifier "$IDENTIFIER" \
    --version "$VERSION" \
    --install-location / \
    "$COMPONENT"

productbuild \
    --package "$COMPONENT" \
    "$OUTDIR/Everwas-Agent-$VERSION.pkg"

printf '%s\n' "build-pkg: wrote $OUTDIR/Everwas-Agent-$VERSION.pkg (unsigned)"
