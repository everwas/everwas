# ADR-0004: The remediation VLAN is required, certificates are short, and routine supersession is expiry not revocation

Status: accepted (2026-08-18)

Amends [ADR-0003](0003-certificate-authority-and-8021x.md), which assumed a
quarantine VLAN without requiring one, and left "CRL only, or OCSP as well?" as
an open question. This settles both, and the second answer falls out of the
first.

## Context

Three findings, in the order they turned up.

**Renewal was not bounding anything.** The agent generates a fresh key on every
renewal rather than re-certifying the old one, on the stated reasoning that a
key which outlives many certificates makes the certificate's expiry the only
bound on how long a stolen key stays useful. That argument quietly assumes the
superseded certificate stops working. It does not. `issue_for_device` records
the new certificate beside the old ones and retires nothing, so a device
accumulates valid certificates, any of which will authenticate it. One test VM
was holding three. The fresh key bought nothing.

**Revocation, as built, cannot be seen by anyone.** The issued certificates
carry no CRL Distribution Point and nothing serves the CRL that `build_crl`
produces. "Revoke" writes a timestamp in our own database that no verifier can
observe.

**Wiring it up as written would be worse than leaving it broken.** l2trace does
support CRL checking, as `RADIUS_EAP_TLS_CRL`, and its own comment carries the
warning that matters: a CRL whose `nextUpdate` has passed fails EVERY
certificate, so a revocation list that stops being republished takes 802.1X
down with it. Our `CRL_LIFETIME` is 24 hours. Connecting those two creates a
mechanism where one day of failed publication drops the entire fleet off the
network at once, triggered by a stopped job or a full disk rather than by an
attacker.

Weigh the two failures against each other. Without revocation, a stolen key
stays useful for the certificate's remaining life instead of half of it, and
stealing it requires SYSTEM or root on the machine or physical possession of
the disk, so the blast radius is one device identity. With a CRL, an ordinary
operational lapse takes down authentication fleet-wide. The safety mechanism's
failure mode is far worse than the failure it prevents, which is the sign that
the mechanism is wrong rather than that its parameters need tuning.

## The load-bearing question

Everything above hangs on one thing: what the network does to a machine that
fails 802.1X.

ADR-0003 assumed the answer. It says a device that fails "lands in a quarantine
VLAN", and builds on that: management stays on wss/443 because quarantine
usually permits it, the packages endpoint is public because a provisioning VLAN
often permits exactly one host. But nothing required that VLAN to exist, nothing
specified what it must permit, and l2trace does not implement one. It is in
monitor mode, where every reply is an Access-Accept with no VLAN, and the
enforcement design rejects outright with no remediation path.

So the assumption load-bearing for three separate design decisions was never a
requirement, and the code that depends on it would have failed in a way that
looks like something else entirely.

## Decision

### 1. The remediation VLAN is a deployment REQUIREMENT, not an assumption.

An 802.1X failure assigns the port to a remediation VLAN rather than rejecting
it. That VLAN must permit, at minimum, DNS, DHCP, and HTTPS to the Everwas
server. It does not need to permit anything else, and should not.

This is switch and RADIUS configuration Everwas does not control, which is
exactly why it has to be stated as a prerequisite and checked, rather than
assumed by three different components independently.

The consequence worth naming: an unauthenticated device can reach the
management plane from remediation. That is the standard NAC trade and it is
acceptable here, because enrollment needs a token, certificate issuance needs
the agent credential, and the packages endpoint serves signed artifacts and was
always intended to be public. What an attacker gains on that segment is the
ability to download an installer and be refused.

### 2. Certificates are short, and expiry is the routine retirement mechanism.

Once a failure lands a device somewhere it can reach the server, an expired
certificate is no longer a truck roll. It is a device that drops to
remediation, renews over 443, and reauthenticates. That single change removes
the reason certificates were long.

Lifetime drops from 90 days to 30, renewal stays at half life. A superseded
certificate then dies on its own within 30 days with no CRL, no publication
pipeline, and no fleet-wide failure mode. That is a shorter exposure window
than CRL-based revocation-on-renewal would have produced, and it is simpler.

The floor on how short is not the holiday laptop any more, because remediation
recovers that. It is how long Everwas itself may be unavailable: renewal at
half life gives 15 days in which the server can be down before any device is
affected, and a device that is affected lands in remediation rather than
nowhere. Deployments that want it shorter can set it; the agent reads the
window from the certificate it was issued and adapts without a change.

### 3. Routine supersession does NOT use the CRL.

Revocation is reserved for incident response: a key believed stolen, a device
retired or sold. Those are rare, deliberate, and worth the operational weight.
They are not what a Tuesday-afternoon renewal is.

The reason code matters when we do revoke. X.509 distinguishes `superseded`
from `keyCompromise`, and using them properly keeps "revoked" meaning
"something was wrong" rather than becoming a sea of routine lifecycle events
nobody reads, which is the same alarm-fatigue argument that governs the agent's
logging.

If a CRL is ever published, `nextUpdate` must be decoupled from publication
cadence: publish often, but set `nextUpdate` weeks out, so real revocations
propagate quickly while a lapse in publication gives weeks of slack instead of
a day. The current 24-hour value is the dangerous setting and must not be
carried forward unexamined.

### 4. The agent reports the certificate it is actually holding.

The heartbeat carries the serial and expiry of the certificate on disk,
alongside the version and uptime it already reports.

This is detection, not enforcement, and it carries no availability risk, which
is why it is worth doing regardless of anything above. It tells us when a
renewal half-failed and a device is still on the old certificate; it exposes a
machine restored from a backup or cloned from an image, because it reports a
serial we superseded; and it answers "what is this device actually using",
which today is unanswerable. If retirement is ever automated, this is also the
confirmation signal that makes it safe, because the rule becomes *never retire
the serial a device says it is holding*, which makes stranding structurally
impossible rather than merely unlikely.

## Consequences

Bootstrap gets a defined path for the first time. A machine with no certificate
cannot do EAP-TLS at all, and previously had nowhere to land. It now lands in
remediation, reaches the server, enrolls, is issued a certificate and
reauthenticates. The packages endpoint's premise, that a device on a
restricted VLAN can reach exactly one host and that host is its management
server, becomes true by construction rather than by hope.

Deployments without a remediation VLAN are explicitly unsupported for 802.1X.
They can still run Everwas; they cannot safely run short certificates, and they
should not enable CRL checking either, because both failure modes need the
remediation path to be recoverable. The honest position for such a deployment
is longer certificates and detection rather than prevention.

The l2trace side needs the remediation VLAN implemented in its enforcement
phase, which is not built yet. Until it is, certificate lifetime should stay
long: shortening it before the recovery path exists inverts the risk this ADR
is built on.

`_looks_like_machine_auth` in l2trace is PEAP-shaped and returns False for
every EAP-TLS session, which matters more once EAP-TLS is the primary method.
Tracked separately.
