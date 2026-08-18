# Acquiring a Windows code signing certificate

OpenRMM has no code signing certificate yet, so the release workflow refuses
to publish Windows artifacts. This is what closing that gap involves.

Nothing here is a workaround. A self-signed certificate does not help: it
produces exactly the same "Unknown publisher" dialog as no signature at all,
plus the false comfort of a green tick in a build log. The pipeline is built
so an unsigned Windows artifact cannot ship by accident, and the way past that
is a real certificate, not a flag.

## Why it is not optional

An unsigned agent is worse than a missing agent.

SmartScreen blocks it on download and again on execution, with a dialog whose
default is "Don't run". A good share of endpoint protection products quarantine
unsigned binaries that install services, which is precisely what this one
does; some do it silently. Windows Defender Application Control and any
WDAC/AppLocker publisher rule cannot express "allow this agent" at all without
a publisher to name.

The worst outcome is not the block, though: it is what the operator learns.
Getting an unsigned RMM agent onto a fleet means telling admins to click past
a publisher warning, and to tell their colleagues to do the same. That habit
outlives the release. An agent that installs itself as a service and can run
arbitrary scripts is the last software that should be teaching it.

## OV or EV

Since 1 June 2023 the CA/Browser Forum baseline requirements have made the
storage rule the same for both: the private key must live in a FIPS 140-2
Level 2 (or Common Criteria EAL4+) hardware module. There is no "download the
.pfx and put it in a secret" option for a newly issued certificate of either
kind. Anyone describing that workflow is describing a certificate issued
before mid-2023.

What still differs is SmartScreen reputation.

**EV** binds reputation to the certificate. Binaries signed with it are
trusted by SmartScreen from the first download, which for an agent that gets
pushed to a fleet in a single afternoon is the whole point. It costs
substantially more (commonly in the high hundreds per year), requires a
stricter organisational vetting process, and traditionally arrives as a
physical USB token, which is awkward for CI and is why the cloud signing
services below exist.

**OV** costs less and validates less. Its reputation starts at zero and
accrues per binary as installs accumulate, so the first releases after
switching to OV will still be flagged, and every new version resets some of
that. For software distributed at low volume to a small number of managed
fleets, "accrues with download volume" can mean it never really accrues.

For an RMM agent, EV or an EV-equivalent cloud service is the sensible target.
An OV certificate is a real improvement over nothing (EDR products care about
the presence and validity of a signature more than about its class, and
publisher-based allowlisting starts working), but it will not stop the
SmartScreen prompt on day one.

## The realistic options

Because the key cannot be a file, the practical shapes are a hardware token
or a signing service. This pipeline drives all of them through jsign, which
runs on the Linux release runner.

| Option | Shape | Fits CI | Notes |
| --- | --- | --- | --- |
| Azure Artifact Signing (was Trusted Signing) | Microsoft-run service, FIPS 140-2 L3 | yes | Cheapest by a wide margin, roughly ten dollars a month at the entry tier. Certificates are short-lived and rotate automatically, which timestamping makes a non-issue. Organisations need a verifiable trading history (three years, US/Canada at the time of writing); individual developers have a separate verification path. Check current eligibility before planning around it. |
| DigiCert KeyLocker, SSL.com eSigner, GlobalSign | Vendor cloud HSM, OV or EV | yes | The usual route to an EV certificate that a build server can actually use. Priced per year, several hundred and up. |
| USB token (YubiKey, SafeNet eToken) | Physical | no | Someone has to plug it into the machine that signs. Fine for hand-cut releases, not for a tagged-push workflow, unless a self-hosted runner is on offer. |
| Cloud KMS (AWS, Google, Azure Key Vault) | HSM-backed key, certificate from a CA | yes | Works, but you still have to buy the certificate; the KMS only solves storage. |

Prices and eligibility rules move. Treat the table as a shape guide and check
the vendor before committing.

## Wiring it in

The workflow reads four secrets, all of which must be set before a release
will run:

| Secret | Value |
| --- | --- |
| `OPENRMM_SIGN_BACKEND` | jsign store type: `TRUSTEDSIGNING`, `DIGICERTONE`, `ESIGNER`, `AZUREKEYVAULT`, `AWS`, `GOOGLECLOUD`, `PKCS11`, `YUBIKEY`, `PKCS12` |
| `OPENRMM_SIGN_KEYSTORE` | service endpoint, PKCS#11 config file, or keystore path |
| `OPENRMM_SIGN_ALIAS` | certificate alias within the keystore |
| `OPENRMM_SIGN_SECRET` | keystore password or API token |

For Azure Artifact Signing that reads as backend `TRUSTEDSIGNING`, keystore
the regional endpoint (`https://eus.codesigning.azure.net`), alias
`<account>/<certificate-profile>`, and secret an access token for a service
principal with the Trusted Signing Certificate Profile Signer role. For
DigiCert KeyLocker it is backend `DIGICERTONE`, alias the certificate alias,
and the secret the pipe-separated API key, client certificate and its
password that jsign expects. jsign's own documentation is authoritative for
each backend's exact format.

`OPENRMM_SIGN_TSA` overrides the timestamp authority, which defaults to
DigiCert's. Do not disable timestamping. Without an RFC 3161 countersignature
every signature stops validating the day the certificate expires, including
on agents already installed across the fleet, and the failure arrives as a
fleet that stops being trusted rather than as a build error.

Once a certificate exists, set `OPENRMM_SIGN_CAFILE` in the release job to the
issuing root and `verify-signature.sh` will additionally require the chain to
validate. It is left off until then because a Linux CA bundle has no opinion
about Microsoft's code signing roots, and a chain check against it would fail
every release for a reason unrelated to the artifact.

## What the pipeline does in the meantime

`.github/workflows/release.yml` fails at the gate step with a message naming
the missing secrets and pointing here, in the same shape as the existing
minisign gate. It does not produce an unsigned artifact and label it.

To exercise the rest of the pipeline before a certificate exists, run the
workflow manually with `allow_unsigned` checked. It publishes nothing, the
signature checks are skipped rather than faked, and the artifact bundle is
named `dist-UNSIGNED-DO-NOT-DEPLOY` because an artifact zip outlives the run
page someone downloaded it from.
