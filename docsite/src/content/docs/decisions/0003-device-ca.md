---
title: "ADR-0003: A device CA, EAP-TLS in l2trace, and posture-gated network access"
description: Everwas issues device certificates, a companion RADIUS server verifies them, and the management credential stays deliberately separate.
sidebar:
  label: "0003 · Device CA & 802.1X"
  order: 3
---

**Status: accepted** (2026-08-18; proposed 2026-08-17), amended by
[ADR-0004](/decisions/0004-short-lived-certificates/), which turns the
quarantine-VLAN assumption into a requirement and settles the CRL
question.

Companion project: [l2trace](https://l2trace.warehack.ing), which is the
RADIUS server in these deployments, not a bystander alongside NPS or
FreeRADIUS.

## Context

Two problems arrived together and turned out to be the same problem.

**The rotation lockout.** Credential rotation keeps the old secret alive
for 24 hours, then pushes the new one to the agent over NATS. Nothing
retries. A laptop switched off for a long weekend boots on Monday
holding a secret that expired on Saturday, with no channel left to
receive its replacement. The bug is not the grace window; it is that
delivery is a **push** to a machine that may not be there.

**802.1X as a first-class capability.** We want Everwas to provision and
manage the certificates endpoints use for EAP-TLS network
authentication, which means running a certificate authority and a full
certificate lifecycle.

The connection is that certificate renewal is conventionally **pull**:
the endpoint asks for a new credential using the one it still holds.
That property, not the certificate itself, is what removes the lockout.

## Decision

**1. Everwas issues; l2trace verifies.** Everwas runs the device CA and
signs CSRs. l2trace holds the trust anchor and consumes a CRL. The
signing key never goes near the RADIUS service: issuance needs an
authenticated channel to the endpoint, which only Everwas has, and a
network-facing service holding a signing key is privilege it does not
need.

**2. The management credential is separate from the network
certificate.** Different credential, different intermediate, different
EKU and lifetime. If one credential did both, revoking a device's
network access would simultaneously revoke the ability to reach it and
fix it: a self-inflicted truck roll at precisely the moment access
matters most. A corollary: **agent management stays on wss/443**,
because a device that fails 802.1X lands in a quarantine VLAN, and
quarantine VLANs normally permit web egress while rarely permitting
arbitrary ports.

**3. Renewal is agent-initiated and verified before the old credential
dies.** The agent asks for its replacement using the credential it
currently holds. Nothing is ever pushed to a machine that might be
absent.

## Accepted costs

- **Per-platform supplicant work is the biggest lift**, larger than the
  certificate handling: wpa_supplicant plus NetworkManager on Linux, CNG
  and Wired AutoConfig on Windows, profiles plus keychain on macOS.
- **CA key custody must be answered before any code.** Root offline with
  an online intermediate is the shape, and the self-hosted default has
  to be safe by default.
- **A device off-network longer than its certificate lifetime is still
  locked out.** Long lifetimes, renewal at half-life, and early alarms
  make this rare, not impossible. It is stated here so it is not
  discovered.
- **Clock skew becomes a support surface.** A wrong clock fails EAP-TLS
  in a way that looks like a certificate problem; NTP health should
  become a monitored fact with an alarm.
- **Devices without the agent cannot bootstrap this way.** Printers,
  cameras, and phones need MAB or SCEP; out of scope, and the gap is
  deliberate.

## Rejected alternatives

- **Client certificates for the agent's own NATS connection.** TLS
  terminates at Caddy, so a client certificate cannot be handed to NATS
  for authorization; fixing that reverses ADR-0001 and breaks
  remediation from a quarantine VLAN.
- **Certificates replacing the shared secret entirely.** This regresses
  revocation: the auth-callout makes revoking an agent one database
  flip, effective immediately, while certificate revocation means CRL
  distribution and a window.
- **A CA in l2trace.** Superficially the natural home; rejected on
  privilege, per decision 1.

## Sequencing

1. Agent-initiated renewal for the existing secret (closes the live
   lockout now, and is the same endpoint shape the CSR flow needs).
2. CA, issuance, CRL publication, with key custody decided first.
3. EAP-TLS (type 13) in l2trace.
4. Per-platform key store and supplicant profiles.
5. Posture into the RADIUS policy context, and CoA-verified renewal.

Steps 1 and 2 are Everwas. Step 3 is l2trace. Steps 4 and 5 are the
seam. The full record, including the open questions on key custody,
certificate lifetime, and CRL versus OCSP, is in `docs/adr/0003` in the
repository.
