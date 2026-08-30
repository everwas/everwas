---
title: 802.1X network authentication
description: "Deploy EAP-TLS with the certificates Everwas issues: the remediation VLAN you must build first, the supplicant profile per platform, and a rollout order that cannot take a site offline."
---

[Device certificates](/guides/certificates/) covers issuance and renewal:
the CA, the key that never leaves the machine, the drift report. This
guide is the other half, the part that happens on the network. It assumes
issuance is already switched on, that you run a RADIUS server that speaks
EAP-TLS (in these deployments,
[l2trace](https://l2trace.warehack.ing)), and that you can change switch
configuration.

Read the first section before you enable anything. It is the one piece
Everwas cannot build for you, and every other decision here rests on it.

## First, the remediation VLAN

An 802.1X failure must **assign the port to a remediation VLAN rather
than rejecting it**, and that VLAN must permit at minimum DNS, DHCP, and
HTTPS to the Everwas server. It should permit nothing else.
[ADR-0004](/decisions/0004-short-lived-certificates/) makes this a
requirement rather than an assumption, and the reason is arithmetic
rather than principle.

Without a remediation path, a machine whose certificate expired is a
machine that cannot reach the server that would issue it a new one. That
is a physical visit, per machine, and the machines fail in batches
because they were imaged in batches. With one, the same failure is a
device that drops to remediation, renews over 443, and reauthenticates
without anyone driving anywhere. The same path is what makes bootstrap
work at all: a machine that has never had a certificate cannot do EAP-TLS
by definition, and it has to land somewhere it can enroll from.

What this costs is that an unauthenticated device can reach the
management plane. That is the standard NAC trade, and it is a small one
here: enrollment still needs a token, certificate issuance still needs
the agent credential, and the packages endpoint serves signed artifacts.
What an attacker gains on that segment is the ability to download an
installer and be refused.

Deployments without a remediation VLAN are explicitly unsupported for
802.1X. You can still run Everwas, and you can still issue and track
certificates; what you cannot safely do is enforce on them, or shorten
them, or turn on CRL checking. The honest position there is longer
certificates and detection rather than prevention.

### What that means for certificate lifetime

`EVERWAS_CA_CERT_LIFETIME_DAYS` defaults to 90, and the agent renews at
half of whatever it was issued, reading the window from the certificate
itself. Ninety days is the value for a deployment where an expired
certificate is a truck roll.

Drop it to 30 only once remediation is actually enforcing at the switch,
not when it is merely planned. Then a superseded certificate retires
itself within a month with no CRL and none of the fleet-wide failure
modes CRL publication carries.

Do not go shorter without doing the sum, because the floor is not the
holiday laptop. Remediation recovers that one. The floor is **how long
the Everwas server itself may be unavailable**: renewal at half life is
the entire margin, so a 30-day certificate gives the server 15 days to be
down before any device is affected, and a device that is affected lands
in remediation rather than nowhere.

## What the device presents

The agent generates a P-256 key locally, under `netcert/` in its state
directory, and sends only a certificate signing request. The subject in
that CSR is a placeholder: the server sets the Common Name **from the
credential the agent authenticated with**, because a CSR is
attacker-controlled input and a device does not get to choose whose
identity its key carries.

So the CN is the device UUID, the same string the console shows. The
generated supplicant profile uses that same string as the EAP identity,
which means the RADIUS `User-Name` arrives as the bare UUID with nothing
prepended, and a session in the auth log and a device in Everwas are one
lookup rather than a mapping table you have to maintain.

The leaf carries `clientAuth` as a critical extended key usage and does
not carry `serverAuth`. That is deliberate: a device certificate that
could also authenticate a server is one that can be lifted off a stolen
laptop and used to stand up a rogue access point that every supplicant in
the estate trusts. You can confirm it on any issued leaf:

```bash
openssl verify -CAfile chain.pem -purpose sslclient network.crt   # OK
openssl verify -CAfile chain.pem -purpose sslserver network.crt   # unsuitable certificate purpose
```

The second failing is the point. Three files live in `netcert/`:

| File | Mode | What it is |
|---|---|---|
| `network.key` | `0600` | The private key. Never transmitted, and referenced by path rather than embedded anywhere |
| `network.crt` | `0644` | The leaf, readable by the supplicant |
| `network-chain.pem` | `0644` | Intermediate then root, the order a verifier expects |

## Who provides the identity

On a domain-joined Windows machine, Active Directory may already do
everything this guide describes: AD CS autoenrollment issues a machine
certificate into the same store, and Group Policy pushes the 802.1X
profile. Two systems then believe they own the same thing, and the
failure modes are quiet rather than loud. A Group Policy machine profile
takes precedence over one added with `netsh`, so the agent can write a
profile, report success, and change nothing; and with two client-auth
certificates in the store, whichever one Windows picks may not be the
one your RADIUS server trusts, which surfaces as an authentication
rejection pointing at nothing.

So the agent detects an existing identity provider and, by default,
stands aside. What a machine does about 802.1X is a mode with three
values:

| Value | Meaning |
|---|---|
| `auto` (the default when nobody has decided) | Defer to whatever is already provisioning this machine; provide an identity on a clean one |
| `always` | Provide an identity even beside AD. For migrating a fleet from AD CS to Everwas, where both are deliberately present for a while |
| `never` | Provide none. For a site whose Windows estate is entirely AD-provisioned and would rather not have a heuristic deciding |

### Set it once for the organization

The common case is a site that runs Active Directory everywhere and
wants one answer for the estate, not a decision repeated per host and
forgotten on the next machine somebody images. So the policy is set on
the organization, and there is a step before setting it, because this
setting is safe per machine and dangerous per fleet without the words
changing between the two. `never` on one machine is a considered
decision; `never` across an organization lets every machine currently
using an Everwas certificate keep working and then drop off the network
as that certificate expires, one at a time, over the following weeks.
Nothing errors at the moment of the change; the failure arrives a month
later, spread out, looking like something else.

First, ask what the change would cost:

```text
GET /api/v1/devices/network-identity/preview?mode=never
```

The preview changes nothing. It returns whether the mode is `safe`, how
many machines would lose network access and how many would not, the
window the losses fall inside (`earliest_loss` to `latest_loss`), and
the affected machines themselves, each with the hostname, the moment it
loses access, and the certificate serial it is holding. It reads what
each device **reports holding** rather than what the server last issued
it, because a machine that never installed its newest certificate loses
access when the older one it is actually using expires, which is
sooner. Machines holding no certificate are not counted; they have no
access to lose.

Then set it, acknowledging the cost:

```text
POST /api/v1/devices/network-identity
{"mode": "never", "acknowledge_affected": 12}
```

The server refuses with a 409 unless `acknowledge_affected` matches
what the change would cost *right now*. This is a count rather than a
confirmation flag on purpose: a flag is something a client sets once
and forgets, while a count can only come from somebody who fetched the
preview, and it stops matching when the fleet moves underneath them,
which forces a fresh look rather than approval of a remembered picture.
A harmless change must be acknowledged too (`"acknowledge_affected": 0`),
so no client grows a code path that skips the field. The response is
the same shape as the preview: what you just committed to, not an
acknowledgement that says nothing.

### How it reaches the fleet

Agents **pull** the policy on the credential renewal they already make,
at startup and on a timer, and re-read their effective mode every cycle
rather than capturing it at startup. Nothing is pushed, for the same
reason nothing else here is pushed: a push reaches every machine except
the ones in an odd state already, which are exactly the machines that
matter. A fleet-wide change reaches machines within a renewal cycle,
not on their next reboot.

### The per-machine escape hatch

`network_identity` in a machine's own config file overrides the
organization policy, always. The reason to set one is that something is
wrong with *this* machine, and changing the fleet to fix one machine
would be the wrong move. The agent keeps the fleet policy it last
pulled in a separate field underneath (`network_identity_policy`,
maintained by the agent; do not edit it), so removing a local override
falls back to what the organization decided rather than to the default,
and "the org chose `auto`" stays distinct from "nobody has decided" the
whole way down.

A mistyped local value is an error in the log, not a silent fallback to
the wrong intention; the agent then runs `auto`, the one mode that
cannot take over a machine by accident.

The rule underneath is an asymmetry: **detection may never stop us, an
operator may**. If Everwas is already this machine's identity source,
finding Active Directory beside it does not make the agent defer,
because detection can be wrong and deferring would stop renewing a
certificate the machine is actively using; that certificate would
expire, and an expired 802.1X certificate takes the machine off the
network with no remote way back. An explicit `never` on that same
machine *is* honoured, because that is somebody's decision rather than a
heuristic misfiring, and it is logged as a warning naming the
consequence, since the person who set it may not have known this machine
was one of ours.

The same detection is also reported, not just acted on: the
[`8021x-identity-source` posture check](/reference/posture-checks/)
shows which system owns each machine's identity in the console, and it
fails only for the one arrangement worth surfacing, both systems
provisioning the same machine at once.

## Give the RADIUS server the trust anchor

The RADIUS server needs the Everwas device CA chain as a trust anchor for
client certificates. It lives under the server's `ca_dir` (`/data/ca` by
default) as `chain.pem`.

Note which direction each chain runs, because getting it backwards
produces a deployment that authenticates and trusts the wrong thing. Our
chain verifies **clients**. The RADIUS server's own certificate is signed
by a different authority, and that authority is what the supplicant
validates the server against. The device CA deliberately does not sign
the RADIUS server's leaf: two chains, one blast radius each.

## Generate the supplicant profile

```bash
everwas-agent supplicant-profile              # wired
everwas-agent supplicant-profile --ssid corp  # wireless
```

The command refuses to do anything useful in the two cases where the
output would be misleading. An agent that is not enrolled has no identity
to present, and a device holding no certificate would get a
syntactically fine profile that fails the handshake with an error about
the certificate, which sends whoever reads it looking at the CA rather
than at the machine that never obtained one.

The file lands in the agent state directory (`--out DIR` to put it
elsewhere) as `wpa_supplicant-everwas.conf`, written through a temporary
file and renamed into place so a supplicant reading it mid-write cannot
get half a config.

Wired and wireless are different files, not the same file with a name
added:

```ini
ctrl_interface=/run/wpa_supplicant
eapol_version=2
ap_scan=0
fast_reauth=1

network={
	key_mgmt=IEEE8021X
	eap=TLS
	identity="0198c4d2-3f7a-7b21-9c55-2f0e6a1b4d33"
	ca_cert="/etc/everwas/netcert/network-chain.pem"
	client_cert="/etc/everwas/netcert/network.crt"
	private_key="/etc/everwas/netcert/network.key"
}
```

`ap_scan=0` appears only for wired, and it is the single most common
reason a hand-written wired profile never authenticates. The default
tells wpa_supplicant to scan for access points and pick a network; on a
wired interface it finds none, so it sits there having done nothing wrong
and nothing at all. There is no error to find, which is why this is worth
knowing before you spend an afternoon on it.

The other pair worth naming: wired uses `key_mgmt=IEEE8021X` and wireless
uses `WPA-EAP`. Using the wireless value on a wired interface fails in a
way that reads like a certificate problem, a long way from the actual
cause.

There is no `private_key_passwd`, because the key is protected by file
permissions rather than a passphrase. A passphrase that has to be
readable by an unattended supplicant is not a passphrase.

### Linux

Test it in the foreground, against the switch it will actually face,
before anything is made permanent:

```bash
wpa_supplicant -c /etc/everwas/wpa_supplicant-everwas.conf -i eth0 -D wired
```

Making it permanent is your init system's business, not the agent's: a
`wpa_supplicant@.service` unit pointed at this file, or a NetworkManager
connection carrying the same settings. The agent will not do it for you,
and the next section explains why that is the intended behaviour.

### Windows takes three steps, not two

This is the least-known part of a Windows 802.1X rollout, and the middle
step is the one that catches people.

**One: get the certificate into the machine store.** The native
supplicant does not read PEM files from a directory. It takes its client
credential from the certificate store, and the profile says *which*
certificate to use rather than where the bytes are. Bundle the key and
leaf into a PKCS#12 and import into `LocalMachine\My`, with the chain
into the machine's trusted roots:

```powershell
# from the PEM material in C:\ProgramData\Everwas\Agent\netcert
openssl pkcs12 -export -inkey network.key -in network.crt `
  -certfile network-chain.pem -out device.pfx -passout pass:temp

Import-PfxCertificate -FilePath device.pfx -CertStoreLocation Cert:\LocalMachine\My `
  -Password (ConvertTo-SecureString temp -AsPlainText -Force)
```

**Two: start Wired AutoConfig.** The `dot3svc` service consumes LAN
profiles, and on Windows it ships **Stopped, with startup type Manual**.
Until it is running, `netsh` refuses to add a profile at all, and the
refusal does not mention the service:

```powershell
Set-Service dot3svc -StartupType Automatic
Start-Service dot3svc
```

**Three: add the profile.**

```powershell
netsh lan add profile filename="C:\ProgramData\Everwas\Agent\everwas-8021x.xml" interface="Ethernet"
```

Read it back with `netsh lan show profiles`. A correct one reports:

```text
802.1x                    : Enabled
802.1x                    : Not Enforced
EAP type                  : Microsoft: Smart Card or other certificate
802.1X auth credential    : Machine credential
```

Two of those lines are decisions rather than defaults.

**Machine credential**, because the certificate is a device identity: its
Common Name is the device UUID and it is issued to the machine, not to
whoever is signed in. `authMode` of `user` would look for a credential in
a user store that does not exist, and the machine would fail to
authenticate whenever nobody was logged in, which is most of the time for
a server and all of the time at the login screen.

**Not Enforced**, because enforcing means a machine that cannot
authenticate has no network at all, including no route to the server that
would fix it. That is the opposite of the remediation posture this whole
design rests on. Enforcement belongs on the switch, where a failure lands
the device in remediation, not on the endpoint, where a failure lands it
nowhere.

The profile also disables the user prompt for server validation, because
a machine authenticating at the login screen has nobody to click "trust
this server", and a prompt nobody can answer is a machine that never
authenticates while looking like a certificate problem. If you pin the
RADIUS server by thumbprint, note that Windows prints thumbprints spaced
in some tools and unspaced in others; the spaced form produces a profile
netsh accepts and that never matches a server, which is the worst
possible combination.

### The Windows profile today

The agent carries the profile generator, and its output is what was
tested against netsh, but `supplicant-profile` writes the wpa_supplicant
form on every platform: there is no Windows branch on the subcommand yet.
Until there is, this is the XML to save and hand to `netsh lan add
profile`.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<LANProfile xmlns="http://www.microsoft.com/networking/LAN/profile/v1">
	<MSM>
		<security>
			<OneXEnforced>false</OneXEnforced>
			<OneXEnabled>true</OneXEnabled>
			<OneX xmlns="http://www.microsoft.com/networking/OneX/v1">
				<authMode>machine</authMode>
				<EAPConfig>
					<EapHostConfig xmlns="http://www.microsoft.com/provisioning/EapHostConfig">
						<EapMethod>
							<Type xmlns="http://www.microsoft.com/provisioning/EapCommon">13</Type>
							<VendorId xmlns="http://www.microsoft.com/provisioning/EapCommon">0</VendorId>
							<VendorType xmlns="http://www.microsoft.com/provisioning/EapCommon">0</VendorType>
							<AuthorId xmlns="http://www.microsoft.com/provisioning/EapCommon">0</AuthorId>
						</EapMethod>
						<Config xmlns="http://www.microsoft.com/provisioning/EapHostConfig">
							<Eap xmlns="http://www.microsoft.com/provisioning/BaseEapConnectionPropertiesV1">
								<Type>13</Type>
								<EapType xmlns="http://www.microsoft.com/provisioning/EapTlsConnectionPropertiesV1">
									<CredentialsSource>
										<CertificateStore>
											<SimpleCertSelection>true</SimpleCertSelection>
										</CertificateStore>
									</CredentialsSource>
									<ServerValidation>
										<DisableUserPromptForServerValidation>true</DisableUserPromptForServerValidation>
										<ServerNames></ServerNames>
									</ServerValidation>
									<DifferentUsername>false</DifferentUsername>
								</EapType>
							</Eap>
						</Config>
					</EapHostConfig>
				</EAPConfig>
			</OneX>
		</security>
	</MSM>
</LANProfile>
```

Type 13 is EAP-TLS, and it appears twice on purpose; 25 would be PEAP,
which is the method this is most easily confused with.
`SimpleCertSelection` lets Windows pick the machine certificate whose
issuer and EKU fit, which is safe here precisely because our leaf is
`clientAuth`-critical with no `serverAuth`: there is exactly one
certificate in the store it can mean.

## Generating a profile does not apply it

The agent writes a file and stops. It does not start wpa_supplicant, does
not reload a running one, does not touch NetworkManager, and does not
call netsh. That is the design, not a missing feature.

Generating a profile is safe. Applying one is not. Telling a machine to
start authenticating on a network that is not expecting it is how a fleet
goes offline in a single push, and it goes offline in exactly the way
that removes your ability to undo it. So applying stays a deliberate
decision, made once per site, after the profile has been tested against
that site's own switches.

If you automate it later, automate it your way, on your rollout schedule,
with your own rollback. What the agent gives you is the part that is
identical on every machine and tedious to get right by hand.

## A rollout order that cannot take a site offline

The order matters more than the individual steps.

1. **Leave RADIUS in monitor mode.** l2trace defaults to it: every reply
   is an Access-Accept, no VLAN is assigned, and the result is recorded.
   Everything below can then be wrong without anyone losing a network.
2. **Issue certificates fleet-wide and wait.** Issuance touches nothing
   about connectivity. Let it run until the [drift
   report](/guides/certificates/) is quiet and you know what every
   machine holds.
3. **Apply a profile to one machine**, on the switch model you actually
   have, with a console cable or a second path to it. Confirm the auth
   log names the device UUID.
4. **Build remediation and prove it,** by failing a machine on purpose:
   pull its certificate, watch the port land in remediation, watch the
   agent renew over 443 and reauthenticate. This is the step people skip,
   and it is the only one that tells you whether recovery works before
   you need it.
5. **Then enforce**, one switch at a time.
6. **Then, and only then, shorten certificate lifetime.**

Reversing steps 4 and 5 is how a site discovers that its remediation VLAN
had no DNS.

## Verifying and watching

The auth log should show the device UUID as `User-Name`, matching the
console exactly. If you see something prepended or a hostname instead,
the profile identity is not the one the agent generated.

Clock skew deserves a monitor of its own. Certificate validation is
time-sensitive, and a machine with a wrong clock fails EAP-TLS in a way
that looks exactly like a certificate problem, which sends people to the
CA to debug NTP.

For what devices are actually holding as opposed to what you issued them,
`GET /api/v1/devices/certificate-drift` lists the mismatches, classified
`stale` (holds an older one we issued: a renewal that half-failed, or a
restore from a backup taken before it), `unknown` (holds a serial we
never issued: a clone, a template, or something worse), and `missing`
(we issued, the device holds nothing). Devices that have never reported a
serial are skipped rather than flagged, so a fleet mid-upgrade does not
light up entirely.

## One note for agents older than the rename

Machines still running an agent from when the project was called OpenRMM
migrate their own state directory on first start of the new build and do
not need re-enrolling; the [enrollment
page](/getting-started/enroll-an-agent/) has the detail.

The part that matters here is that `netcert/` lives inside that
directory. If you ever move agent state by hand rather than letting the
migration do it, move the whole directory: copying `agent.json` alone
leaves a machine enrolled and managed while silently dropping its network
identity, and it surfaces weeks later as an authentication failure nobody
can account for.
