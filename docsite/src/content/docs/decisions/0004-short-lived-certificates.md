---
title: "ADR-0004: The remediation VLAN is required, certificates are short, and routine supersession is expiry not revocation"
description: A CRL whose publication lapses takes the whole fleet offline; short certificates plus a guaranteed remediation path retire the mechanism that could.
sidebar:
  label: "0004 · Short certs, no routine CRL"
  order: 4
---

**Status: accepted** (2026-08-18)

Amends [ADR-0003](/decisions/0003-device-ca/), which assumed a quarantine
VLAN without requiring one, and left "CRL only, or OCSP as well?" open.
This settles both, and the second answer falls out of the first.

## Context

Three findings, in the order they turned up.

**Renewal was not bounding anything.** The agent generates a fresh key on
every renewal, on the stated reasoning that certificate expiry should be
the only bound on how long a stolen key stays useful. That argument
quietly assumes the superseded certificate stops working. It did not:
issuance recorded the new certificate beside the old ones and retired
nothing, so a device accumulated valid certificates, any of which would
authenticate it. One test VM was holding three.

**Revocation, as built, could not be seen by anyone.** Issued
certificates carried no CRL Distribution Point and nothing served the CRL
being built. "Revoke" wrote a timestamp in our own database that no
verifier could observe.

**Wiring it up as written would be worse than leaving it broken.** The
RADIUS side does support CRL checking, with the warning that matters: a
CRL whose `nextUpdate` has passed fails *every* certificate, so a
revocation list that stops being republished takes 802.1X down with it.
Against a 24-hour CRL lifetime, one stopped job or full disk drops the
entire fleet off the network at once.

Weigh the two failures. Without revocation, a stolen key stays useful for
the certificate's remaining life instead of half of it, and stealing it
requires root on the machine or possession of the disk: blast radius, one
device identity. With a CRL, an ordinary operational lapse takes down
authentication fleet-wide. When the safety mechanism's failure mode is
worse than the failure it prevents, the mechanism is wrong, not its
parameters.

## The load-bearing question

Everything above hangs on what the network does to a machine that fails
802.1X. ADR-0003 assumed the answer: it said a failing device "lands in a
quarantine VLAN" and built three separate design decisions on top, but
nothing required that VLAN to exist, nothing specified what it must
permit, and the RADIUS side did not implement one.

## Decision

**1. The remediation VLAN is a deployment requirement, not an
assumption.** An 802.1X failure assigns the port to a remediation VLAN
rather than rejecting it. That VLAN must permit, at minimum, DNS, DHCP,
and HTTPS to the Everwas server, and should permit nothing else. It is
switch and RADIUS configuration Everwas does not control, which is
exactly why it is stated as a prerequisite and checked rather than
assumed. The trade is standard NAC: an unauthenticated device can reach
the management plane from remediation, where enrollment still needs a
token, issuance still needs the agent credential, and the packages
endpoint serves signed artifacts. What an attacker gains is the ability
to download an installer and be refused.

**2. Certificates are short, and expiry is the routine retirement
mechanism.** Once a failure lands a device somewhere it can reach the
server, an expired certificate is no longer a truck roll: the device
drops to remediation, renews over 443, and reauthenticates. Lifetime
drops from 90 days to 30, renewal at half life. A superseded certificate
dies on its own within 30 days with no CRL, no publication pipeline, and
no fleet-wide failure mode. The floor on how short is no longer the
holiday laptop (remediation recovers it); it is how long Everwas itself
may be down, and renewal at half life gives the server 15 days of slack.

**3. Routine supersession does not use the CRL.** Revocation is reserved
for incident response: a key believed stolen, a device retired or sold.
Rare, deliberate, worth the weight. The X.509 reason codes are used
properly (`superseded` versus `keyCompromise`) so "revoked" keeps meaning
"something was wrong". If a CRL is ever published, `nextUpdate` must be
decoupled from publication cadence: publish often, set `nextUpdate` weeks
out, so real revocations propagate quickly while a publication lapse
gives weeks of slack instead of a day.

**4. The agent reports the certificate it is actually holding.** The
heartbeat carries the serial and expiry of the certificate on disk. This
is detection, not enforcement, with no availability risk: it exposes
half-failed renewals, machines restored from backups or cloned from
images (a serial we superseded), and answers "what is this device
actually using". If retirement is ever automated, this is the signal
that makes it safe, because the rule becomes *never retire the serial a
device says it is holding*.

## Consequences

Bootstrap gets a defined path for the first time: a machine with no
certificate lands in remediation, reaches the server, enrolls, is issued
a certificate, and reauthenticates.

Deployments without a remediation VLAN are explicitly unsupported for
802.1X. They can still run Everwas; they cannot safely run short
certificates and should not enable CRL checking either, because both
failure modes need the remediation path to be recoverable. The honest
position there is longer certificates and detection rather than
prevention.

The RADIUS-side remediation VLAN is not built yet. Until it is,
certificate lifetime stays long: shortening it before the recovery path
exists inverts the risk this record is built on.
