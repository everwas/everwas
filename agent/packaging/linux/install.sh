#!/bin/sh
# OpenRMM agent installer for Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/openrmm/agent/main/packaging/linux/install.sh \
#     | sudo OPENRMM_SERVER=https://rmm.example.com OPENRMM_TOKEN=abc123 sh
#
# Environment:
#   OPENRMM_SERVER   server base URL, enrollment is skipped when unset
#   OPENRMM_TOKEN    one-time enrollment token
#   OPENRMM_VERSION  release tag to install, defaults to the latest release
#   OPENRMM_REPO     GitHub owner/repo, defaults to openrmm/agent
#   OPENRMM_PREFIX   install prefix for the binary, defaults to /usr/local/bin
set -eu

REPO="${OPENRMM_REPO:-openrmm/agent}"
PREFIX="${OPENRMM_PREFIX:-/usr/local/bin}"
VERSION="${OPENRMM_VERSION:-}"
SERVER="${OPENRMM_SERVER:-}"
TOKEN="${OPENRMM_TOKEN:-}"

WORKDIR=""

log() { printf '%s\n' "openrmm: $*"; }
die() { printf '%s\n' "openrmm: $*" >&2; exit 1; }

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
    [ -n "$tag" ] && [ "$tag" != "latest" ] || die "could not determine the latest release; set OPENRMM_VERSION"
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

    asset="openrmm-agent_${plain_version}_linux_${arch}.tar.gz"
    base="https://github.com/$REPO/releases/download/$VERSION"

    WORKDIR="$(mktemp -d)"
    log "downloading $asset"
    curl -fsSL -o "$WORKDIR/$asset" "$base/$asset" || die "download failed: $base/$asset"
    curl -fsSL -o "$WORKDIR/SHA256SUMS" "$base/SHA256SUMS" || die "download failed: $base/SHA256SUMS"

    verify_sha256 "$WORKDIR/$asset" "$WORKDIR/SHA256SUMS"

    tar -xzf "$WORKDIR/$asset" -C "$WORKDIR" openrmm-agent
    [ -f "$WORKDIR/openrmm-agent" ] || die "archive did not contain openrmm-agent"
    chmod 0755 "$WORKDIR/openrmm-agent"

    mkdir -p "$PREFIX"
    if [ -n "$SERVER" ] && [ -n "$TOKEN" ]; then
        "$WORKDIR/openrmm-agent" install --path "$PREFIX/openrmm-agent" --server "$SERVER" --token "$TOKEN"
    else
        log "OPENRMM_SERVER or OPENRMM_TOKEN not set, installing without enrolling"
        "$WORKDIR/openrmm-agent" install --path "$PREFIX/openrmm-agent"
    fi

    log "done. Check status with: $PREFIX/openrmm-agent status"
}

main "$@"
