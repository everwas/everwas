#!/bin/sh
# Everwas agent installer for Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/everwas/agent/main/packaging/linux/install.sh \
#     | sudo EVERWAS_SERVER=https://rmm.example.com EVERWAS_TOKEN=abc123 sh
#
# Environment:
#   EVERWAS_SERVER   server base URL, enrollment is skipped when unset
#   EVERWAS_TOKEN    one-time enrollment token
#   EVERWAS_VERSION  release tag to install, defaults to the latest release
#   EVERWAS_REPO     GitHub owner/repo, defaults to everwas/agent
#   EVERWAS_PREFIX   install prefix for the binary, defaults to /usr/local/bin
set -eu

REPO="${EVERWAS_REPO:-everwas/agent}"
PREFIX="${EVERWAS_PREFIX:-/usr/local/bin}"
VERSION="${EVERWAS_VERSION:-}"
SERVER="${EVERWAS_SERVER:-}"
TOKEN="${EVERWAS_TOKEN:-}"

WORKDIR=""

log() { printf '%s\n' "everwas: $*"; }
die() { printf '%s\n' "everwas: $*" >&2; exit 1; }

cleanup() {
    # Only ever remove a directory this script created. An unset or empty
    # WORKDIR must never reach rm.
    if [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ]; then
        rm -rf -- "$WORKDIR"
    fi
}
trap cleanup EXIT INT TERM

require_root() {
    uid="$(id -u 2>/dev/null || echo 1)"
    if [ "$uid" -ne 0 ]; then
        die "must run as root. Re-run with sudo:
  curl -fsSL https://raw.githubusercontent.com/$REPO/main/packaging/linux/install.sh | sudo sh"
    fi
}

need() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

detect_arch() {
    machine="$(uname -m)"
    case "$machine" in
        x86_64 | amd64) echo amd64 ;;
        aarch64 | arm64) echo arm64 ;;
        *) die "unsupported architecture: $machine (amd64 and arm64 are published)" ;;
    esac
}

detect_distro() {
    # Sourced in a subshell on purpose: /etc/os-release defines VERSION, which
    # would otherwise clobber the release tag this script is installing.
    if [ -r /etc/os-release ]; then
        (
            # shellcheck disable=SC1091
            . /etc/os-release
            printf '%s %s' "${NAME:-linux}" "${VERSION_ID:-unknown}"
        )
        return
    fi
    uname -s
}

latest_version() {
    # The redirect on /releases/latest carries the tag, which avoids needing a
    # JSON parser and avoids the low rate limit on the API host.
    url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")"
    tag="${url##*/}"
    [ -n "$tag" ] && [ "$tag" != "latest" ] || die "could not determine the latest release; set EVERWAS_VERSION"
    printf '%s' "$tag"
}

verify_sha256() {
    file="$1"
    sums="$2"
    name="$(basename "$file")"
    line="$(grep -E "[ *]${name}\$" "$sums" || true)"
    [ -n "$line" ] || die "$name is not listed in SHA256SUMS"

    expected="${line%% *}"
    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$file" | cut -d' ' -f1)"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$file" | cut -d' ' -f1)"
    else
        die "no sha256sum or shasum available to verify the download"
    fi
    [ "$expected" = "$actual" ] || die "checksum mismatch for $name
  expected $expected
  actual   $actual"
    log "checksum verified: $name"
}

main() {
    require_root
    need curl
    need tar
    need grep

    arch="$(detect_arch)"
    if [ -z "$VERSION" ]; then
        VERSION="$(latest_version)"
    fi
    # Release tags are vX.Y.Z; asset names drop the leading v.
    plain_version="${VERSION#v}"

    log "distro:  $(detect_distro)"
    log "arch:    $arch"
    log "version: $VERSION"

    asset="everwas-agent_${plain_version}_linux_${arch}.tar.gz"
    base="https://github.com/$REPO/releases/download/$VERSION"

    WORKDIR="$(mktemp -d)"
    log "downloading $asset"
    curl -fsSL -o "$WORKDIR/$asset" "$base/$asset" || die "download failed: $base/$asset"
    curl -fsSL -o "$WORKDIR/SHA256SUMS" "$base/SHA256SUMS" || die "download failed: $base/SHA256SUMS"

    verify_sha256 "$WORKDIR/$asset" "$WORKDIR/SHA256SUMS"

    tar -xzf "$WORKDIR/$asset" -C "$WORKDIR" everwas-agent
    [ -f "$WORKDIR/everwas-agent" ] || die "archive did not contain everwas-agent"
    chmod 0755 "$WORKDIR/everwas-agent"

    mkdir -p "$PREFIX"
    if [ -n "$SERVER" ] && [ -n "$TOKEN" ]; then
        "$WORKDIR/everwas-agent" install --path "$PREFIX/everwas-agent" --server "$SERVER" --token "$TOKEN"
    else
        log "EVERWAS_SERVER or EVERWAS_TOKEN not set, installing without enrolling"
        "$WORKDIR/everwas-agent" install --path "$PREFIX/everwas-agent"
    fi

    log "done. Check status with: $PREFIX/everwas-agent status"
}

main "$@"
