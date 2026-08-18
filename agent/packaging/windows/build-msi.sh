#!/usr/bin/env bash
# Build the Everwas agent MSI from an already-signed everwas-agent.exe.
#
#   build-msi.sh --exe dist/.../everwas-agent.exe --arch amd64 \
#                --version 2026.08.17 --out dist
#
# Called from .goreleaser.yaml as a post-build hook on the Windows build, so
# the exe it wraps is the same file that goes into the release zip, signature
# and all. Runs standalone too.
#
# Needs: wixl and msiinfo/msibuild (msitools), plus whatever sign.sh needs.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exe="" arch="" version="" out="dist"

while [ $# -gt 0 ]; do
  case "$1" in
    --exe) exe="$2"; shift 2 ;;
    --arch) arch="$2"; shift 2 ;;
    --version) version="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    *) echo "build-msi.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$exe" ] && [ -n "$arch" ] && [ -n "$version" ] || {
  echo "build-msi.sh: --exe, --arch and --version are required" >&2
  exit 2
}
[ -f "$exe" ] || { echo "build-msi.sh: no such file: $exe" >&2; exit 1; }

for tool in wixl msiinfo msibuild; do
  command -v "$tool" >/dev/null || {
    echo "build-msi.sh: $tool not found. Install msitools (apt-get install msitools)." >&2
    exit 1
  }
done

# wixl targets x86, x64 and intel, and nothing else: `wixl -a arm64` answers
# "arch of type 'arm64' is not supported". Windows 11 on ARM runs both the x64
# MSI and the x64 agent under emulation, so an ARM host is covered by the x64
# package or by the zip and `everwas-agent.exe install`; it is a performance
# footnote, not a gap. Exiting 0 here rather than failing keeps a Windows
# arm64 build from taking the release down with it, and the release workflow
# asserts separately that the x64 MSI did get built and signed, so this cannot
# quietly become "no MSI at all".
case "$arch" in
  amd64|x86_64|x64) wixl_arch=x64 ;;
  arm64|aarch64)
    echo "build-msi.sh: no MSI for windows/$arch (wixl cannot target arm64); the zip is the install path there" >&2
    exit 0
    ;;
  *) echo "build-msi.sh: unsupported arch $arch" >&2; exit 2 ;;
esac

# ---------------------------------------------------------------------------
# CalVer -> MSI ProductVersion.
#
# MSI compares only the first three fields of ProductVersion and caps the
# first at 255, the second at 255 and the third at 65535. A CalVer year does
# not fit in the first field and a fourth field is ignored outright, so
# 2026.08.17 and 2026.08.17.1 would either be rejected or compare equal, and
# a same-day fix would not upgrade the release it fixes.
#
#   2026.08.17    -> 26.8.1700
#   2026.08.17.1  -> 26.8.1701
#
# Day times a hundred plus the same-day patch keeps every release of a year
# strictly increasing, tops out at 3199, and leaves room for 99 fixes a day.
# The real CalVer string still ships, in the Comments summary field and in
# HKLM\SOFTWARE\Everwas\Agent\Version.
# ---------------------------------------------------------------------------
display_version="${version#v}"
core="${display_version%%-*}" # drop a goreleaser snapshot suffix
IFS='.' read -r y m d p <<<"$core"
p="${p:-0}"
case "$y$m$d$p" in
  *[!0-9]*|"") echo "build-msi.sh: cannot read a CalVer out of '$version'" >&2; exit 1 ;;
esac
if [ "$y" -lt 2000 ] || [ "$y" -gt 2255 ] || [ "$m" -lt 1 ] || [ "$m" -gt 12 ] ||
   [ "$d" -lt 1 ] || [ "$d" -gt 31 ] || [ "$p" -gt 99 ]; then
  echo "build-msi.sh: '$version' is not a plausible CalVer (want YYYY.MM.DD[.N], N<100)" >&2
  exit 1
fi
msi_version="$((y - 2000)).$((10#$m)).$((10#$d * 100 + 10#$p))"

# ---------------------------------------------------------------------------
# The exe must already be signed. Building the MSI first and signing "the
# artifacts" afterwards would leave the exe inside the cab unsigned, which is
# the copy that ends up on disk and in front of SmartScreen. Refuse rather
# than produce that.
# ---------------------------------------------------------------------------
if [ "${EVERWAS_ALLOW_UNSIGNED:-}" != "1" ]; then
  "$here/verify-signature.sh" "$exe"
fi

mkdir -p "$out"
msi="$out/everwas-agent_${display_version}_windows_${arch}.msi"

wixl -a "$wixl_arch" \
  -D "Version=$msi_version" \
  -D "DisplayVersion=$display_version" \
  -D "AgentExe=$exe" \
  -o "$msi" \
  "$here/everwas-agent.wxs"

# ---------------------------------------------------------------------------
# Two things wixl parses and then throws away without saying so. Both are
# applied here against the built database and both are checked afterwards,
# because a silent drop is exactly how this got missed the first time.
#
#   SecureCustomProperties  msiexec discards command-line properties that are
#                           not listed here when it hands off to the elevated
#                           install, so /qn SERVER=... TOKEN=... would install
#                           an agent that never enrolls.
#   HideTarget (bit 8192)   keeps the resolved command line, enrollment token
#                           and all, out of the MSI log.
# ---------------------------------------------------------------------------
secure="$(msiinfo export "$msi" Property | awk -F'\t' '$1=="SecureCustomProperties"{print $2}' | tr -d '\r')"
wanted="SERVER;TOKEN;PURGE"
if [ -z "$secure" ]; then
  msibuild "$msi" -q "INSERT INTO \`Property\` (\`Property\`, \`Value\`) VALUES ('SecureCustomProperties', '$wanted')"
else
  msibuild "$msi" -q "UPDATE \`Property\` SET \`Value\` = '$secure;$wanted' WHERE \`Property\` = 'SecureCustomProperties'"
fi

enroll_type="$(msiinfo export "$msi" CustomAction | awk -F'\t' '$1=="RegisterAgentEnroll"{print $2}' | tr -d '\r')"
[ -n "$enroll_type" ] || { echo "build-msi.sh: RegisterAgentEnroll is missing from the MSI" >&2; exit 1; }
if [ $((enroll_type & 8192)) -eq 0 ]; then
  msibuild "$msi" -q "UPDATE \`CustomAction\` SET \`Type\` = $((enroll_type | 8192)) WHERE \`Action\` = 'RegisterAgentEnroll'"
fi

"$here/check-msi.sh" "$msi" "$msi_version" "$display_version"

if [ "${EVERWAS_ALLOW_UNSIGNED:-}" = "1" ]; then
  echo "build-msi.sh: EVERWAS_ALLOW_UNSIGNED=1, leaving $msi unsigned" >&2
else
  "$here/sign.sh" "$msi"
fi

echo "built $msi (ProductVersion $msi_version, agent $display_version)"
