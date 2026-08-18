#!/usr/bin/env bash
# Release gate: open the artifacts an operator will actually download and
# refuse the release unless what is inside them is signed.
#
#   check-release.sh dist
#
# Signing the loose binary and calling it done is how an unsigned agent ships:
# the archive stage can pick up a rebuilt copy, a path can drift, a hook can
# be skipped by a config change nobody read. So this looks inside the zip
# rather than at the file the signing step was handed, and it insists an MSI
# exists at all, because "no MSI was produced" and "the MSI is fine" otherwise
# look identical from outside.
set -euo pipefail

dist="${1:-dist}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[ -d "$dist" ] || { echo "check-release.sh: no such directory: $dist" >&2; exit 1; }
command -v unzip >/dev/null || { echo "check-release.sh: unzip not found" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
checked=0

# Every Windows zip, checked from the inside.
while IFS= read -r zip; do
  d="$tmp/$(basename "$zip" .zip)"
  mkdir -p "$d"
  unzip -q -o "$zip" -d "$d"
  exe="$(find "$d" -name 'openrmm-agent.exe' -print -quit)"
  if [ -z "$exe" ]; then
    echo "check-release.sh: $zip contains no openrmm-agent.exe" >&2
    fail=1
    continue
  fi
  echo "-- inside $(basename "$zip")"
  "$here/verify-signature.sh" "$exe" || fail=1
  checked=$((checked + 1))
done < <(find "$dist" -maxdepth 1 -name '*windows*.zip' -print)

if [ "$checked" -eq 0 ]; then
  echo "check-release.sh: no Windows archives found in $dist" >&2
  fail=1
fi

# At least one MSI, and every MSI signed. x64 only today: wixl cannot target
# arm64, which is a documented gap rather than a build that went missing.
msis=()
while IFS= read -r m; do msis+=("$m"); done < <(find "$dist" -maxdepth 1 -name '*.msi' -print)
if [ "${#msis[@]}" -eq 0 ]; then
  echo "check-release.sh: no MSI in $dist. The Windows install path for" \
       "everyone deploying by GPO or by RMM push is missing." >&2
  fail=1
else
  "$here/verify-signature.sh" "${msis[@]}" || fail=1
fi

[ "$fail" -eq 0 ] || { echo "check-release.sh: this release is not shippable" >&2; exit 1; }
echo "check-release.sh: Windows artifacts in $dist are signed"
