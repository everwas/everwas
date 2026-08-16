#!/bin/sh
# Build an unsigned macOS installer package for the OpenRMM agent.
#
#   ./packaging/darwin/build-pkg.sh dist/openrmm-agent_darwin_universal/openrmm-agent 1.2.3 dist
#
# Produces dist/OpenRMM-Agent-1.2.3.pkg, which drops the binary at
# /Library/OpenRMM/Agent/openrmm-agent and the daemon plist at
# /Library/LaunchDaemons/com.openrmm.agent.plist, then bootstraps the daemon.
#
# NOTARIZATION GAP: this package is unsigned and un-notarized. Gatekeeper will
# refuse it on a normal double click, so today it has to be installed from a
# terminal with `sudo installer -pkg ... -target /`, or through an MDM that
# pushes packages without user interaction. Closing the gap needs an Apple
# Developer ID Installer certificate plus:
#   productsign --sign "Developer ID Installer: ..." unsigned.pkg signed.pkg
#   xcrun notarytool submit signed.pkg --keychain-profile openrmm --wait
#   xcrun stapler staple signed.pkg
# Those steps need credentials that do not belong in this repo, so they run in
# the release pipeline, not here.
set -eu

BINARY="${1:-}"
VERSION="${2:-0.0.0}"
OUTDIR="${3:-dist}"

IDENTIFIER="com.openrmm.agent"
INSTALL_DIR="/Library/OpenRMM/Agent"
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

mkdir -p "$ROOT$INSTALL_DIR" "$ROOT/Library/LaunchDaemons" "$ROOT/Library/Logs/OpenRMM" "$SCRIPTS" "$OUTDIR"

install -m 0755 "$BINARY" "$ROOT$INSTALL_DIR/openrmm-agent"
install -m 0644 "$SCRIPT_DIR/com.openrmm.agent.plist" "$ROOT/Library/LaunchDaemons/$IDENTIFIER.plist"

cat > "$SCRIPTS/postinstall" <<'POSTINSTALL'
#!/bin/sh
set -eu

PLIST=/Library/LaunchDaemons/com.openrmm.agent.plist
STATE=/Library/Application\ Support/OpenRMM/agent.json

mkdir -p /Library/Logs/OpenRMM
chmod 0755 /Library/Logs/OpenRMM

# Only start an agent that has an identity. An unenrolled daemon under
# KeepAlive would respawn every five seconds and fill the log.
if [ -s "$STATE" ]; then
    launchctl bootout system/com.openrmm.agent 2>/dev/null || true
    launchctl bootstrap system "$PLIST" 2>/dev/null || launchctl load -w "$PLIST"
else
    echo "openrmm-agent installed but not enrolled; run:"
    echo "  sudo /Library/OpenRMM/Agent/openrmm-agent install --server URL --token TOKEN"
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
    "$OUTDIR/OpenRMM-Agent-$VERSION.pkg"

printf '%s\n' "build-pkg: wrote $OUTDIR/OpenRMM-Agent-$VERSION.pkg (unsigned)"
