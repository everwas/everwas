#!/usr/bin/env bash
# Authenticode-sign Windows artifacts (.exe, .msi) with jsign.
#
#   sign.sh everwas-agent.exe everwas-agent.msi
#
# Fails when it has no key rather than passing the files through untouched.
# An unsigned agent trips SmartScreen and a good share of EDR products, and
# the operator's way past that is to click through the warning, which is a
# habit worth more to an attacker than this release is to us. A release that
# does not happen is recoverable; teaching a fleet's admins to dismiss
# publisher warnings is not.
#
# jsign rather than osslsigncode because the key almost certainly cannot be a
# file: since June 2023 the CA/Browser Forum baseline requires code signing
# private keys, OV as well as EV, to live in FIPS 140-2 Level 2 hardware or an
# equivalent HSM. jsign talks to the cloud signing services that exist to
# solve that (Azure Trusted Signing, DigiCert ONE, SSL.com eSigner, AWS/Google
# KMS) and to PKCS#11 tokens, from Linux. See docs/windows-code-signing.md.
set -euo pipefail

[ $# -gt 0 ] || { echo "sign.sh: nothing to sign" >&2; exit 2; }

backend="${EVERWAS_SIGN_BACKEND:-}"
keystore="${EVERWAS_SIGN_KEYSTORE:-}"
alias="${EVERWAS_SIGN_ALIAS:-}"
secret="${EVERWAS_SIGN_SECRET:-}"
# RFC 3161 timestamping. Without it every signature made with this
# certificate stops validating the day the certificate expires, including on
# binaries already installed on the fleet.
tsa="${EVERWAS_SIGN_TSA:-http://timestamp.digicert.com}"

missing=()
[ -n "$backend" ] || missing+=(EVERWAS_SIGN_BACKEND)
[ -n "$keystore" ] || missing+=(EVERWAS_SIGN_KEYSTORE)
[ -n "$alias" ] || missing+=(EVERWAS_SIGN_ALIAS)
[ -n "$secret" ] || missing+=(EVERWAS_SIGN_SECRET)
if [ ${#missing[@]} -gt 0 ]; then
  cat >&2 <<EOF
sign.sh: refusing to produce unsigned Windows artifacts.

Missing: ${missing[*]}

  EVERWAS_SIGN_BACKEND   jsign store type: TRUSTEDSIGNING, DIGICERTONE,
                         ESIGNER, AZUREKEYVAULT, AWS, GOOGLECLOUD, PKCS11,
                         YUBIKEY, PKCS12
  EVERWAS_SIGN_KEYSTORE  keystore: the service endpoint, the PKCS#11 config
                         file, or the .p12 path
  EVERWAS_SIGN_ALIAS     certificate alias within the keystore
  EVERWAS_SIGN_SECRET    keystore password or API token
  EVERWAS_SIGN_TSA       timestamp authority (default $tsa)

There is no unsigned fallback on purpose. To build a knowingly unsigned
artifact on a workstation, opt out on purpose with EVERWAS_ALLOW_UNSIGNED=1,
the same way an unsigned self-update build opts out with -X ...DevBuild=true.
That artifact must not leave the workstation.
EOF
  exit 1
fi

# The jar is pinned by the workflow; on a workstation any jsign on PATH does.
if [ -n "${EVERWAS_SIGN_JSIGN_JAR:-}" ]; then
  jsign=(java -jar "$EVERWAS_SIGN_JSIGN_JAR")
elif command -v jsign >/dev/null; then
  jsign=(jsign)
else
  echo "sign.sh: jsign not found. Set EVERWAS_SIGN_JSIGN_JAR or install jsign." >&2
  exit 1
fi

for f in "$@"; do
  [ -f "$f" ] || { echo "sign.sh: no such file: $f" >&2; exit 1; }
  # --storepass is a secret on the command line. jsign has no stdin form, so
  # the exposure is the process list of the runner for the length of one
  # signing call; the alternative, an env-var indirection jsign resolves
  # itself, does not exist either.
  "${jsign[@]}" \
    --storetype "$backend" \
    --keystore "$keystore" \
    --storepass "$secret" \
    --alias "$alias" \
    --alg SHA-256 \
    --tsaurl "$tsa" \
    --tsmode RFC3161 \
    --tsretries 5 \
    --tsretrywait 10 \
    --name "Everwas Agent" \
    --url "https://github.com/rsp2k/everwas" \
    "$f"
done

# Signing that reported success and produced nothing verifiable is the whole
# failure this pipeline exists to prevent, so check before returning.
"$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/verify-signature.sh" "$@"
