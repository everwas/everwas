---
title: Security posture
description: Per-check verdicts with a third answer that is not a verdict, because a check that could not run says nothing about the machine and must never take it off the network.
---

Posture is what the agent can establish about a machine's security state:
whether its disk is encrypted, whether a firewall is up, whether
something is watching for malware. Each of those is a check, and adding a
check is one file plus one line in that platform's list.

The modularity matters less than the thing it protects, which is the
property everything else here is built around: **a check that could not
run must never read as a check that failed.**

## Why that one rule carries the design

Posture is meant to gate network access, and the cost of the two possible
mistakes is nowhere near symmetric.

A missed failure leaves a non-compliant machine on the network, which is
roughly where it already was. Nothing got worse; something did not get
better.

A false failure takes a healthy machine off the network, and it does it
to **every machine that shares the cause**. A vendor tool that changes
its output format, a permission that tightened in an update, a query that
started timing out at 09:00: each of those is one bug in one check,
arriving simultaneously on every machine that runs it. That is how a bad
antivirus query becomes a site-wide outage sixty seconds later, and the
machines it removes are the ones that were fine.

So the collector is allowed to say "I do not know", and saying so is
treated as information rather than as an incomplete answer waiting to be
rounded up into one.

## Three answers, and only two of them are verdicts

On the wire there are three statuses: `pass`, `fail`, and
`not_assessed`. Anything not assessed carries a separate
`not_assessed_reason`, either `not_applicable` or `undetermined`.

The reason is a **separate field on purpose**. Both values mean "no
verdict" to anything making a decision, and keeping the distinction out
of the status field means nothing downstream can promote one of them to a
verdict by matching on the status string alone. A consumer that branches
on status sees three values and cannot accidentally invent a fourth
meaning.

The difference still matters, just to a different reader. `not_applicable`
is permanent and expected: BitLocker on a Linux server is not a gap, and
nobody should ever be asked to close it. `undetermined` is a collection
fault, and it is worth an operator's time, as a data-quality problem
rather than as a compliance one.

## The example that makes it concrete

A Debian server with no firewall tool installed at all reports
`firewall: not_assessed`, reason `undetermined`.

The check asks ufw, then firewalld, then nftables, and none of them are
installed. What it has established is that it did not find a firewall. It
has not established that there is not one: the machine could be running a
fourth thing, or the ruleset could sit behind a permission the agent does
not hold. Those are different facts, and only the first one is true.

Reporting that as `fail` would quarantine a machine for something nobody
ever established, and would do it to every machine at that site running
the same base image. The check that finds ufw installed and inactive
*does* return `fail`, because that is a verdict: it looked, it found the
firewall, and the firewall is off.

The same reasoning runs the other way on Linux antivirus, which reports
`not_applicable`. Linux servers overwhelmingly do not run resident
antivirus, and treating that as non-compliance would fail most of a
normal fleet for doing the normal thing. A site that does mandate an
agent should get a check written for it, rather than have this one
pretend to cover it.

## No single compliant boolean

There is a rollup, and it deliberately does not collapse to one flag.
"Compliant: true/false" cannot represent a machine where two checks
passed, one failed and one could not run, and any collapse has to decide
what an unassessable check means.

That decision belongs to whoever carries the consequence of being wrong,
which is not the collector on the endpoint. A site whose policy is "no
disk encryption verdict means keep it off the finance VLAN" is making a
defensible call. It should be making it deliberately, in its own policy,
not inheriting it from a default somewhere in an agent.

## Posture is stored per check, and the check set grows

Every result lands as its own bitemporal fact, keyed on the check's
stable name. That shape follows directly from the fact that checks are
added over time.

A machine assessed last month was assessed against last month's checks. A
check added since then is not one that machine failed, and it is not one
that machine passed; it is one that **never ran there**. Per-check facts
give it no history before it existed, which is the honest answer. A
whole-machine rollup would have to restate the entire verdict every time
any single check moved, and would have to invent a belief about checks
that had never run on that machine.

The same shape means one check changing amends only its own belief
window, so "what did we think about this machine's firewall in March, and
when did we start thinking it" stays answerable. That is the general
[bitemporal](/concepts/bitemporal/) property applied to a place where the
question comes up during incidents, which is exactly when you cannot
afford an answer that was overwritten.

Absence carries meaning too, and it is the meaning you want: a check with
no rows for a device never ran there. That is different from failing
there, and any consumer treating absence as a failure has the same bug as
one treating `not_assessed` as a failure.

## Categories, and why an empty one is worse than none

Each check declares a category (`encryption`, `malware`, `firewall`), so
a policy can gate on a property rather than on an enumeration of check
names. A policy written against `encryption` keeps covering the machine
when a fifth encryption check is added; one listing `disk-encryption`,
`bitlocker`, `filevault` and `luks` by name silently stops covering it.

Categories are additive-only and are assigned by the check rather than by
the result, for the same reason names are: a category is something
somebody's policy is written against, and a check that reported itself
under different categories on different runs would break that policy
intermittently, which is the worst way for a policy to break.

A category only exists once a check ships in it. An empty category is
worse than an absent one, because an absent category is visibly not
covered while an empty one looks covered and returns nothing. The
platform with no checks at all, currently macOS, publishes no posture
rather than an empty result set, on the same reasoning: "everything was
assessed and nothing was found" and "nothing was assessed" are
indistinguishable once written down, and only one of them is true.

## Where it goes

The agent assesses on the same half-hourly cycle as the rest of
inventory, with each check isolated: its own timeout, and a panic
recovered into `undetermined` rather than allowed to take down the
process that would report it. Results are sorted before hashing so that
map ordering cannot manufacture a change where nothing changed.

From there posture lands as facts on the server and is readable through
[`GET /sync/posture`](/reference/sync-api/), one row per device and
check. The status string passes through untranslated the whole way: the
agent defines the vocabulary, and nothing in the middle gets to decide
what an unassessable check means.

Nothing in Everwas gates network access on posture today. The seam it is
built for is the RADIUS side, where a policy can eventually say "permit
EAP-TLS from a certificate this CA issued, whose device is enrolled,
checked in recently, and has no failing posture check". When that is
wired up, it will be reading three statuses, and it will be reading the
one that is not a verdict as exactly that. The [check
catalogue](/reference/posture-checks/) lists what ships today and what
each check concludes.
