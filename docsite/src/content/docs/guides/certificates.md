---
title: Device certificates for 802.1X
description: Everwas runs a device CA that issues the certificates endpoints present for EAP-TLS network authentication, renewed by the agent before they can strand a machine.
---

Everwas can act as the certificate authority for your network: it issues
each enrolled device a certificate for 802.1X EAP-TLS authentication,
and the agent keeps that certificate renewed for the life of the
machine. The design reasoning lives in
[ADR-0003](/decisions/0003-device-ca/) and
[ADR-0004](/decisions/0004-short-lived-certificates/); this guide is the
operational view.

One separation to hold onto before anything else: the network
certificate is **not** the agent's management credential. Different
credential, different CA chain, different lifetime, on purpose. Revoking
a device's network access must never simultaneously revoke your ability
to reach the device and fix it.

## Enabling issuance

Issuance is off until two things exist: a CA passphrase and CA material.

```bash
# .env
EVERWAS_CA_PASSPHRASE=   # openssl rand -base64 36
```

Back the passphrase up separately from the server. Losing it orphans
every certificate already issued. The server refuses to conjure a CA
into existence with an empty passphrase, on the grounds that a CA nobody
chose a passphrase for is a CA nobody is guarding.

The CA material lives under `/data/ca` (the `ca_dir` setting) and is
created once. Key custody is the part worth understanding:

- The **root key is returned exactly once** at initialization and never
  written to disk. Store it offline; it is only needed again to mint a
  replacement intermediate, which is a rare and deliberate act.
- The **intermediate key is stored encrypted** with the passphrase.
  Day-to-day signing uses it, so a stolen volume or database dump yields
  no usable signing key.
- Re-initialization is deliberately refused if material already exists,
  because it would silently orphan the fleet's certificates.

This is the shape an operator without an HSM can actually run; the
upgrade path is moving the intermediate into a KMS or HSM, which changes
how the key loads and nothing else.

The device CA signs only device certificates. It is deliberately not the
authority that signs your RADIUS server's own certificate: two chains,
one blast radius each.

## How a device gets its certificate

Nothing to do per device. Once issuance is enabled, each agent:

1. generates an ECDSA private key locally, under its state directory.
   The key never leaves the machine; only a certificate signing request
   crosses the wire.
2. submits the CSR over its authenticated HTTPS channel
   (`POST /api/v1/agents/certificate`, using the agent credential).
3. receives the signed certificate and chain, stored beside the key with
   permissions split for the consumer: the supplicant can read the
   certificate and chain, the private key stays locked down (on Windows
   too, via proper ACLs rather than inherited ones).

The certificate's identity is the device id **from the server's own
record of which agent authenticated**, never from the CSR. A CSR is
attacker-controlled input: the device asks for a key to be signed and
does not get to choose whose identity that key carries. Retired devices
are refused outright, since retirement exists to take a machine off the
fleet, not to hand it a fresh network identity through a different door.

Weak keys are refused at issuance, which is the last moment anyone can
still say no.

## The renewal lifecycle

Certificates live 90 days and renew at half life, and the 45-day gap is
the entire safety margin: for 802.1X, an expired certificate locks a
machine off the network, and a machine off the network cannot be
repaired remotely. The weeks between first renewal attempt and expiry
are where failed attempts, alarms, and a human noticing all have to fit.

The agent borrows its escalation shape from DHCP's lease timers:

- **At half life**, renewal is routine: quiet retries, WARN-level logs.
- **At 87.5% of the lifetime** (DHCP's rebinding point), the agent
  concludes the normal path is broken and changes strategy: it retries
  hourly instead of twice a day, logs at ERROR, and tells the person at
  the machine. That last one matters because a device that cannot renew
  is, by definition, a device whose path to the server is broken, so
  every server-side alarm is blind in exactly the case that matters. The
  interruption is rate-limited to once a day and stamped on disk, so a
  crash-looping agent cannot pop a dialog every few seconds.

Fleet renewals are spread with deterministic per-device jitter, so a
hundred machines imaged the same afternoon do not stampede the CA in one
window, and any given machine renews at the same predictable point every
cycle.

Why 90 days rather than shorter: ADR-0004 ties certificate lifetime to
whether an expired certificate strands the machine or merely drops it
into a remediation VLAN that permits reaching the server. Until your
RADIUS enforcement provides that remediation path, lifetime stays long;
shortening it first inverts the risk.

## Knowing what devices actually hold

What you issued and what the machine holds are different facts. They
diverge when a renewal was issued but never saved, when a machine is
restored from a backup image or cloned from a template, and when
material is deleted by hand. So every heartbeat carries the serial and
expiry of the certificate actually on the device's disk, and the server
compares against its issuance record, flagging three kinds of drift:

| Drift | Meaning |
|---|---|
| `missing` | We issued a certificate; the device holds none |
| `stale` | The device holds one we issued, but not the latest |
| `unknown` | The device holds a serial we never issued: a restore, a clone, or something worse |

This is detection, not enforcement, and it is what makes eventual
automation safe: the rule "never retire the serial a device says it is
holding" makes stranding a machine structurally impossible.

## Revocation is for incidents

Routine supersession is handled by expiry, not by the CRL; that is the
core of [ADR-0004](/decisions/0004-short-lived-certificates/). Revoke
for a key believed stolen or a device retired or sold, using the X.509
reason codes properly (`keyCompromise` versus `superseded`), so
"revoked" keeps meaning "something was wrong" instead of becoming a sea
of lifecycle noise. The server can build a CRL for the RADIUS side to
consume; before wiring that up, read the ADR's warning about `nextUpdate`
and fleet-wide failure, because an expired CRL fails every certificate,
not just revoked ones.

## What is not here yet

Installing the certificate into each platform's supplicant
(wpa_supplicant and NetworkManager, the Windows certificate store and
Wired AutoConfig, macOS profiles and keychain) is ADR-0003's step 4 and
is not built yet, and the remediation VLAN on the RADIUS side is a
deployment prerequisite that Everwas cannot provide for you. Until both
exist, treat this as the issuance and lifecycle layer: the certificates
are real, renewed, and tracked, and the network enforcement that
consumes them is the next seam.
