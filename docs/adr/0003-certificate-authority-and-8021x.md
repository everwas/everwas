# ADR-0003: A device CA in OpenRMM, EAP-TLS in l2trace, and posture-gated network access

Status: proposed (2026-08-17)

Companion project: [l2trace](https://l2trace.warehack.ing) (`~/claude/l2trace`),
which is the RADIUS server in these deployments, not a bystander alongside NPS
or FreeRADIUS.

## Context

Two problems arrived together and turned out to be the same problem.

**The rotation lockout.** `rotate_agent_secret` writes a new agent secret and
keeps the old one alive for 24 hours, then the API pushes the new one to the
agent over NATS. Nothing retries. A laptop switched off for a long weekend boots
on Monday holding a secret that expired on Saturday, with no channel left to
receive its replacement. Recovery is a site visit and a fresh enrollment token
per host. The bug is not the grace window; it is that delivery is a **push** to
a machine that may not be there.

**802.1X as a first-class capability.** We want OpenRMM to provision and manage
the certificates endpoints use for EAP-TLS network authentication, which means
running a certificate authority and a certificate lifecycle: issuance, renewal,
revocation, and per-platform installation into the OS key store and supplicant.

The connection is that certificate renewal is conventionally **pull**: the
endpoint asks for a new credential using the one it still holds. That property,
not the certificate itself, is what removes the lockout.

### What already exists

l2trace has more of this built than a survey would suggest:

| Piece | State |
|---|---|
| RADIUS server (auth 1812, acct 1813, CoA 3799) | shipped |
| EAP-TLS/PEAP record layer: flags byte, fragmentation, TLS message length | shipped (`radius/eap.py`) |
| Real TLS handshake over `ssl` MemoryBIO, server side | shipped (`radius/peap.py`) |
| MPPE key derivation for WPA2-Enterprise | shipped (`radius/mppe.py`) |
| PEAP-MSCHAPv2 (password based) | shipped |
| CoA / Disconnect per RFC 5176, and `trigger_reauth` on approval | shipped (`radius/coa.py`, `radius/provisioning.py`) |
| Monitor mode as the DEFAULT (accept, no VLAN, flag only) | shipped |
| Policy engine with `posture_fresh`, `posture_stamped_at`, `identity_group`, `required_auth_method` | shipped, **unpopulated** |
| EAP-TLS as a method (type 13) | **not built** |

The policy engine's own docstring says it: *"In v1 there is no posture assessor,
so `posture_fresh` is always False"*, and *"l2trace has no identity source
today, so `identity_group` is left None ... until a resolver populates it"*.
Those are seams cut for a component that did not exist yet.

OpenRMM is that component. It already knows, bitemporally, whether a device is
enrolled, patched, healthy, when it last checked in, and who is logged into it.

## Decision

### 1. OpenRMM issues. l2trace verifies.

OpenRMM runs the device CA and signs CSRs. l2trace holds the trust anchor and
consumes a CRL. The signing key never goes near the RADIUS service.

Two reasons. Issuance requires an authenticated channel to the endpoint, and
OpenRMM already has exactly one: an enrolled agent on wss/443. l2trace has no
relationship with an endpoint before that endpoint is on the network. And a
network-facing service holding a signing key is privilege it does not need: an
l2trace compromise must not be able to mint identities that join the customer's
network.

### 2. The management credential is SEPARATE from the network certificate.

The agent authenticates to OpenRMM with its own credential over wss/443. The
802.1X certificate is a different credential, from a different intermediate,
with a different EKU and lifetime.

This is not tidiness. If one credential did both, then revoking a device's
network access would simultaneously revoke the ability to reach it and fix it:
a self-inflicted truck roll at precisely the moment access matters most.

The corollary constrains transport: **agent management stays on wss/443**
because a device that fails 802.1X lands in a quarantine VLAN, and quarantine
VLANs normally permit web egress while rarely permitting arbitrary ports. The
choice of 443 in ADR-0001 was justified by corporate egress policy; it is now
also load-bearing for remediation. Moving agent auth to mTLS on 4222 would make
the management plane unreachable exactly when the network is broken.

### 3. Renewal is agent-initiated, and VERIFIED before the old credential dies.

The agent asks for its replacement using the credential it currently holds.
Nothing is ever pushed to a machine that might be absent.

Renewal happens at ~50% of lifetime, not near expiry, so there are weeks of
retries before anything is at risk, and issuance is staggered so a batch does
not expire together.

Then the part this pairing makes possible and almost no PKI deployment can do:
after installing a renewed certificate, OpenRMM asks l2trace to force a
re-authentication via CoA. If the device comes back authenticated on the new
certificate, renewal is **confirmed**. If it fails, the old certificate is still
valid for weeks and the management channel is still up, so it is fixed remotely.

Renewal is otherwise a hope. Here it is a test with a rollback window.

### 4. Bootstrap uses monitor mode, not a special case.

1. Device arrives with no certificate. l2trace is in monitor mode (its default)
   or the port does MAB. Either way the device lands somewhere with web egress.
2. The agent enrolls to OpenRMM over wss/443 with a one-time token.
3. The agent generates a keypair **locally**, TPM-backed and non-exportable
   where the platform allows, and sends a CSR. The private key never leaves the
   device and OpenRMM never sees it.
4. OpenRMM signs a client certificate (EKU `clientAuth`).
5. The agent installs it into the OS key store and writes the supplicant
   profile.
6. OpenRMM tells l2trace the device is cert-capable; l2trace issues a CoA to
   bounce the port.
7. The device re-authenticates with EAP-TLS and lands on its real VLAN.

There is no chicken-and-egg because step 1 does not require a certificate, and
every step but 3-5 already exists in one of the two projects.

### 5. Posture feeds policy.

OpenRMM populates l2trace's `posture_fresh` and `identity_group` for an
endpoint. A policy can then say: permit EAP-TLS from a certificate issued by
the OpenRMM CA, whose device is currently enrolled, has no critical patches
outstanding, and checked in within the hour.

That is posture-based NAC. The interface is deliberately narrow, one boolean
plus a timestamp plus a group label, because `evaluate` is pure and must stay
that way; the freshness computation belongs in `policy_cache.build_auth_context`
where it already lives.

The loop closes both ways: l2trace's `trigger_reauth` becomes the enforcement
arm for an OpenRMM decision, and the OpenRMM agent becomes the remediation arm
for an l2trace quarantine.

### 6. Rotation grace becomes conditional on delivery, except for revocation.

Two rotations exist and they want opposite things:

- **Hygiene / rollover.** The old credential stays valid until the agent proves
  it holds the new one. No wall clock, so no lockout.
- **Revocation.** The old credential dies on a short, fixed deadline, because
  the point is to lock something out.

These are separate operations rather than one with a flag, so an operator
choosing "revoke" is choosing it, not inheriting it from a default.

## Consequences

### Accepted costs

**Per-platform supplicant work is the biggest lift**, larger than the
certificate handling: `wpa_supplicant` plus NetworkManager on Linux, CNG and
the Wired AutoConfig service on Windows, profiles plus keychain on macOS.
Budget accordingly; this is where the schedule will go.

**CA key custody must be answered before any code.** Root offline with an online
intermediate is the shape. The self-hosted default has to be safe, because
otherwise most operators will end up with the signing key in a Docker volume
beside Postgres.

**A device off-network longer than its certificate lifetime is still locked
out.** Long lifetimes, renewal at half-life, staggered issuance and early alarms
make this rare; they do not make it impossible. This is the residual risk and it
should be stated in the docs rather than discovered.

**Clock skew becomes a support surface.** Certificate validation is
time-sensitive and a device with a wrong clock fails EAP-TLS in a way that looks
like a certificate problem. NTP health should become a monitored fact with an
alarm.

**Devices without the agent cannot bootstrap this way.** Printers, cameras and
phones need MAB or SCEP. Out of scope here; noted so the gap is deliberate.

### Rejected alternatives

**Client certificates for the agent's own NATS connection.** TLS terminates at
Caddy (`no_tls: true` on the NATS websocket listener), so a client certificate
is verified by Caddy and cannot be handed to NATS for authorization. Making it
work means terminating TLS at NATS, which reverses ADR-0001 and, worse, breaks
remediation from a quarantine VLAN. Certificates may later be added at Caddy as
a transport-level check (nothing unauthenticated reaches NATS at all), which is
defence in depth and costs nothing architecturally.

**Certificates replacing the shared secret entirely.** This regresses
revocation. The auth-callout was chosen in the M1 design specifically so that
revoking an agent is one database flip, effective immediately. NATS does no
OCSP, so certificate revocation there means CRL distribution and a window.

**A CA in l2trace.** It has `lab-certs/` already and terminates the TLS tunnel,
so it is superficially the natural home. Rejected on privilege: see decision 1.

## Sequencing

1. **Agent-initiated renewal for the existing secret.** Closes the live lockout
   now, and is the same endpoint shape the CSR flow needs. Not throwaway.
2. **CA, issuance, CRL publication.** Key custody decided first.
3. **EAP-TLS (type 13) in l2trace.** The record layer, TLS tunnel and MPPE keys
   already exist; this is the method, client certificate verification, and no
   inner method.
4. **Per-platform key store and supplicant profiles.**
5. **Posture into `AuthContext`**, and CoA-verified renewal.

Steps 1 and 2 are OpenRMM. Step 3 is l2trace. Steps 4 and 5 are the seam.

## Open questions

- Where does the intermediate signing key live for a self-hosted deployment
  that has no HSM or KMS?
- Certificate lifetime: long enough to survive a holiday, short enough that a
  stolen non-TPM key has a bounded life. 90 days with renewal at 45 is the
  starting proposal.
- Does OpenRMM push posture to l2trace, or does l2trace pull it? Push is fresher;
  pull keeps the trust direction one-way, which matters because l2trace is the
  network-facing service.
- CRL only, or OCSP as well? CRL freshness bounds revocation latency for the
  network path.
