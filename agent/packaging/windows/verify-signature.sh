#!/usr/bin/env bash
# Assert that every file given carries a timestamped Authenticode signature.
#
#   verify-signature.sh dist/openrmm-agent_windows_amd64_v1/openrmm-agent.exe
#
# This is the gate, not the signing step's own opinion of itself. A signing
# tool that exits 0 without embedding anything, a config pointed at the wrong
# path, an archive built from the copy that was signed one step too late: all
# of those look like a green release, and are otherwise discovered when a
# customer's endpoint protection quarantines the agent.
#
# Two checks, both of which fail the build:
#
#   presence   osslsigncode extract-signature exits non-zero when there is no
#              signature to extract. Unlike `osslsigncode verify` this does
#              not consult a trust store, which matters: an Azure Trusted
#              Signing certificate chains to a Microsoft root that is not in a
#              Linux CA bundle, so a chain check here would fail every release
#              for a reason that has nothing to do with the artifact.
#   timestamp  a signature with no RFC 3161 countersignature stops validating
#              the day the certificate expires, on binaries already installed
#              across the fleet. That is a silent time bomb, so an
#              untimestamped artifact is treated as unsigned.
#
# Set OPENRMM_SIGN_CAFILE to additionally require the chain to validate
# against a specific root, once it is known which root that is.
set -euo pipefail

[ $# -gt 0 ] || { echo "verify-signature.sh: nothing to verify" >&2; exit 2; }

osslsigncode="${OPENRMM_OSSLSIGNCODE:-osslsigncode}"
command -v "$osslsigncode" >/dev/null || {
  echo "verify-signature.sh: osslsigncode not found (apt-get install -y osslsigncode)" >&2
  exit 1
}

# 1.3.6.1.4.1.311.3.3.1 is the RFC 3161 timestamp token attribute;
# 1.2.840.113549.1.9.6 is the older Authenticode countersignature. Either one
# means the signature outlives the certificate.
tsoid_rfc3161="1.3.6.1.4.1.311.3.3.1"
tsoid_authenticode="1.2.840.113549.1.9.6"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

status=0
for f in "$@"; do
  if [ ! -f "$f" ]; then
    echo "verify-signature.sh: no such file: $f" >&2
    status=1
    continue
  fi

  sig="$tmp/sig.p7b"
  rm -f "$sig"
  if ! "$osslsigncode" extract-signature -in "$f" -out "$sig" >"$tmp/log" 2>&1; then
    echo "UNSIGNED: $f" >&2
    sed 's/^/    /' "$tmp/log" >&2
    status=1
    continue
  fi

  subject="$(openssl pkcs7 -inform DER -in "$sig" -print_certs -noout 2>/dev/null |
    grep -m1 '^subject=' || true)"

  oids="$(openssl asn1parse -inform DER -in "$sig" 2>/dev/null || true)"
  if ! printf '%s' "$oids" | grep -qF -e "$tsoid_rfc3161" -e "$tsoid_authenticode"; then
    echo "NOT TIMESTAMPED: $f (${subject:-unknown signer})" >&2
    status=1
    continue
  fi

  if [ -n "${OPENRMM_SIGN_CAFILE:-}" ]; then
    if ! "$osslsigncode" verify -CAfile "$OPENRMM_SIGN_CAFILE" -in "$f" >"$tmp/log" 2>&1; then
      echo "CHAIN DOES NOT VALIDATE: $f" >&2
      sed 's/^/    /' "$tmp/log" >&2
      status=1
      continue
    fi
  fi

  echo "signed + timestamped: $f ${subject#subject=}"
done
exit $status
