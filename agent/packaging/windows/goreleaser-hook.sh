#!/usr/bin/env bash
# GoReleaser post-build hook: Authenticode-sign the Windows binary and wrap it
# in an MSI, before the archive stage sees it.
#
#   goreleaser-hook.sh --os windows --arch amd64 --path dist/.../everwas-agent.exe \
#                      --version 2026.08.17
#
# The ordering is the point. GoReleaser's own binary_signs stage runs after
# every build, which is too late to get a signed exe into the cab of an MSI
# built from it; a build hook is the last place where the binary is a loose
# file that both the archive and the installer will pick up. Signing after the
# fact would leave the copy inside the zip and inside the MSI unsigned, which
# are the two copies that actually land on a host.
#
# Nothing to do for linux and darwin. That is not the guarantee that Windows
# artifacts get signed; the guarantee is check-release.sh, which opens the
# published zip and the MSI and fails the release if what is inside them is
# unsigned.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
os="" arch="" path="" version=""

while [ $# -gt 0 ]; do
  case "$1" in
    --os) os="$2"; shift 2 ;;
    --arch) arch="$2"; shift 2 ;;
    --path) path="$2"; shift 2 ;;
    --version) version="$2"; shift 2 ;;
    *) echo "goreleaser-hook.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done

[ "$os" = "windows" ] || exit 0

[ -n "$arch" ] && [ -n "$path" ] && [ -n "$version" ] || {
  echo "goreleaser-hook.sh: --arch, --path and --version are required" >&2
  exit 2
}

if [ "${EVERWAS_ALLOW_UNSIGNED:-}" = "1" ]; then
  echo "goreleaser-hook.sh: EVERWAS_ALLOW_UNSIGNED=1, $path stays unsigned; do not ship it" >&2
else
  "$here/sign.sh" "$path"
fi

# dist is where GoReleaser's checksum and release stages look, and
# checksum.extra_files picks the MSI up from there.
"$here/build-msi.sh" --exe "$path" --arch "$arch" --version "$version" --out dist
