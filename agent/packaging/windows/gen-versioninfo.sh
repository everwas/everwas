#!/usr/bin/env bash
# Generate the Windows VERSIONINFO resource that gets linked into the agent.
#
# Not cosmetic. A Go binary carries no version resource by default, so Windows
# Installer classifies it as an UNVERSIONED file, and for those it falls back to
# a heuristic: if the modified time differs from the created time the file looks
# like something a user edited, and MSI PRESERVES it rather than overwriting.
#
# Observed on a real Windows 11 VM, in the installer's own log:
#
#   File: C:\Program Files\Everwas\Agent\everwas-agent.exe;
#   Won't Overwrite; Won't patch; Existing file is unversioned but modified
#
# The install reported success, the service restarted, and the binary on disk
# was the old one. A major upgrade forces the component in anyway, so the
# MSI-to-MSI path was never affected; the case this fixes is a first MSI install
# on top of an agent that was installed by hand or by script, which is exactly
# what migrating an existing fleet looks like.
#
# With a version resource present MSI compares versions instead of guessing from
# timestamps, and the guess goes away.
set -euo pipefail

version="${1:?usage: gen-versioninfo.sh <CalVer> [outdir]}"
outdir="${2:-cmd/everwas-agent}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# CalVer maps to the four 16-bit fields Windows allows. Same mapping as the MSI
# ProductVersion so the two cannot disagree about which build is newer.
IFS='.' read -r y m d patch <<<"${version}"
patch="${patch:-0}"
major=$(( 10#${y} % 100 ))
minor=$(( 10#${m} ))
build=$(( 10#${d} * 100 + 10#${patch} ))

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
sed -e "s/\"Major\": 0, \"Minor\": 0, \"Patch\": 0, \"Build\": 0/\"Major\": ${major}, \"Minor\": ${minor}, \"Patch\": ${build}, \"Build\": 0/g" \
    "${here}/../../cmd/everwas-agent/versioninfo.json" > "$tmp"

go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.7.0 \
  -o "${outdir}/resource_windows_amd64.syso" -arm=false -64 \
  -product-version "${version}" -file-version "${version}" "$tmp"

echo "gen-versioninfo.sh: wrote ${outdir}/resource_windows_amd64.syso (${major}.${minor}.${build}.0 from ${version})"
