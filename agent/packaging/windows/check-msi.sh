#!/usr/bin/env bash
# Assert the invariants of a built OpenRMM agent MSI.
#
#   check-msi.sh openrmm-agent.msi 26.8.1700 2026.08.17
#
# wixl is quiet about the parts of the WiX v3 schema it does not implement:
# Property/@Secure, CustomAction/@HideTarget and Before/After sequencing are
# all parsed and discarded without a word. Each of those failures produces a
# package that builds, installs, and is wrong in a way nobody notices until a
# fleet-wide rollout enrolls nothing or a token turns up in a support log.
# So the shape of the database is checked here rather than assumed.
set -euo pipefail

msi="${1:?usage: check-msi.sh MSI MSI_VERSION DISPLAY_VERSION}"
want_version="${2:?}"
want_display="${3:?}"

# Must match Product/@UpgradeCode in openrmm-agent.wxs. Changing it orphans
# every installed agent: MSI would no longer see the old package as the same
# product, so an upgrade turns into a second Add/Remove entry fighting over
# C:\Program Files\OpenRMM\Agent.
readonly UPGRADE_CODE="{C715EB57-9DA9-4A77-AD9E-FA4A78DE65F0}"

fail=0
bad() { echo "check-msi: $*" >&2; fail=1; }

# msiinfo export emits three header lines (names, types, table+keys) then
# CRLF-terminated rows.
table() { msiinfo export "$msi" "$1" | tail -n +4 | tr -d '\r'; }
prop() { table Property | awk -F'\t' -v k="$1" '$1==k{print $2}'; }
seq_of() { table InstallExecuteSequence | awk -F'\t' -v k="$1" '$1==k{print $3}'; }
ca_type() { table CustomAction | awk -F'\t' -v k="$1" '$1==k{print $2}'; }

[ "$(prop ProductVersion)" = "$want_version" ] ||
  bad "ProductVersion is '$(prop ProductVersion)', expected '$want_version'"
[ "$(prop UpgradeCode)" = "$UPGRADE_CODE" ] ||
  bad "UpgradeCode is '$(prop UpgradeCode)', expected '$UPGRADE_CODE'"
[ -n "$(prop ProductCode)" ] || bad "no ProductCode"
[ "$(prop ALLUSERS)" = "1" ] || bad "ALLUSERS is not 1, the package would install per-user"

# Without these three in SecureCustomProperties, msiexec drops them when it
# elevates and /qn SERVER=... TOKEN=... installs an agent that never enrolls.
secure="$(prop SecureCustomProperties)"
for p in SERVER TOKEN PURGE; do
  case ";$secure;" in
    *";$p;"*) ;;
    *) bad "$p is not in SecureCustomProperties ('$secure')" ;;
  esac
done

case ";$(prop MsiHiddenProperties);" in
  *";TOKEN;"*) ;;
  *) bad "TOKEN is not in MsiHiddenProperties; the token would be written to the MSI log" ;;
esac

# Type bits: 18 exe-from-File-table, 1024 deferred, 2048 no-impersonate,
# 8192 hide-target, 64 ignore-return.
check_ca() {
  local action="$1" want_bits="$2" reject_bits="$3" t
  t="$(ca_type "$action")"
  [ -n "$t" ] || { bad "custom action $action is missing"; return; }
  [ $((t & want_bits)) -eq "$want_bits" ] ||
    bad "$action type $t is missing bits $want_bits"
  [ "$reject_bits" -eq 0 ] || [ $((t & reject_bits)) -eq 0 ] ||
    bad "$action type $t must not have bits $reject_bits"
}
check_ca RegisterAgent $((18 | 1024 | 2048)) 64
check_ca RegisterAgentEnroll $((18 | 1024 | 2048 | 8192)) 64
check_ca RollbackRegisterAgent $((18 | 1024 | 2048 | 256)) 0
check_ca UnregisterAgent $((18 | 1024 | 2048)) 64
check_ca PurgeAgentIdentity $((18 | 1024 | 2048)) 0

# A deferred action outside InstallInitialize..InstallFinalize gets error 2762
# at run time, which is what wixl's document-order numbering produced before
# every Custom element was given an explicit Sequence.
init="$(seq_of InstallInitialize)"
final="$(seq_of InstallFinalize)"
removefiles="$(seq_of RemoveFiles)"
installfiles="$(seq_of InstallFiles)"
for a in RegisterAgent RegisterAgentEnroll RollbackRegisterAgent UnregisterAgent PurgeAgentIdentity; do
  s="$(seq_of "$a")"
  [ -n "$s" ] || { bad "$a is not sequenced"; continue; }
  { [ "$s" -gt "$init" ] && [ "$s" -lt "$final" ]; } ||
    bad "$a at $s is outside InstallInitialize($init)..InstallFinalize($final)"
done

# The SCM holds the service image open, so deregistration has to happen before
# RemoveFiles or the uninstall leaves files behind and asks for a reboot.
for a in UnregisterAgent PurgeAgentIdentity; do
  [ "$(seq_of "$a")" -lt "$removefiles" ] ||
    bad "$a runs at or after RemoveFiles($removefiles)"
done
# ...and registration after the exe it runs has been laid down.
for a in RollbackRegisterAgent RegisterAgentEnroll RegisterAgent; do
  [ "$(seq_of "$a")" -gt "$installfiles" ] ||
    bad "$a runs at or before InstallFiles($installfiles)"
done
# Removing the old product late would try to overwrite an exe the SCM still
# has open.
rep="$(seq_of RemoveExistingProducts)"
{ [ -n "$rep" ] && [ "$rep" -gt "$init" ] && [ "$rep" -lt "$installfiles" ]; } ||
  bad "RemoveExistingProducts at '${rep:-unset}' is not between InstallInitialize($init) and InstallFiles($installfiles)"

# The rollback record has to be written before the actions it undoes.
[ "$(seq_of RollbackRegisterAgent)" -lt "$(seq_of RegisterAgentEnroll)" ] ||
  bad "RollbackRegisterAgent is not sequenced before RegisterAgentEnroll"

table File | grep -q 'openrmm-agent\.exe' || bad "openrmm-agent.exe is not in the File table"
# Attribute 256 is msidbComponentAttributes64bit. Without it a 64-bit package
# still resolves ProgramFiles64Folder but writes the registry to the WOW6432
# view, and an inventory query would not find the version.
#
# Shell arithmetic rather than awk's and(): that is a gawk extension, and the
# CI runner's awk is mawk, where it is a syntax error rather than a false
# result.
while IFS=$'\t' read -r comp _ _ attrs _; do
  [ -n "$comp" ] || continue
  [ $((attrs & 256)) -ne 0 ] || bad "component $comp is not marked 64-bit"
done < <(table Component)

table Registry | grep -qF "$want_display" ||
  bad "HKLM\\SOFTWARE\\OpenRMM\\Agent Version does not carry '$want_display'"

msiinfo streams "$msi" | grep -q '\.cab$' || bad "the cab is not embedded in the MSI"

[ "$fail" -eq 0 ] || { echo "check-msi: $msi is not shippable" >&2; exit 1; }
echo "check-msi: $msi looks correct (ProductVersion $want_version, agent $want_display)"
