---
title: Posture checks and wire format
description: The three statuses, the two not-assessed reasons, the fields on a result, and every check that ships today with what it concludes.
---

The reasoning behind this shape is in [security
posture](/concepts/security-posture/); this page is the contract. If you
are writing something that consumes posture, the rule that governs every
table below is that only `fail` is a failure.

## Statuses

| Status | Meaning |
|---|---|
| `pass` | The check ran and the machine is in the desired state |
| `fail` | The check ran and the machine is not. The only value that is a failure |
| `not_assessed` | No verdict. Carries `not_assessed_reason` |

`not_assessed` is a single status with a reason beside it rather than two
statuses, so that a consumer matching on the status string cannot promote
one of them to a verdict. A value you do not recognise is not assessed;
it is never a failure.

| `not_assessed_reason` | Meaning |
|---|---|
| `not_applicable` | The check does not mean anything on this machine and never will. Permanent and expected, not a gap to close |
| `undetermined` | The check applies but could not be determined: a tool was missing, a command failed, a permission was denied, a check timed out or panicked. A collection problem worth fixing, and never evidence of non-compliance |

## Result fields

| Field | Type | Notes |
|---|---|---|
| `check` | string | The stable identifier, e.g. `disk-encryption`. Renaming one costs every record that came before, so it does not happen |
| `category` | string | `encryption`, `malware`, or `firewall`. Set from the check, not per result, so a check cannot report itself under different categories on different runs. Additive-only |
| `status` | string | As above |
| `not_assessed_reason` | string | Present only when the status is `not_assessed` |
| `detail` | string | One sentence for a human reading a console: why this status |
| `evidence` | object | String pairs a machine can act on, e.g. which firewall backend answered, which profiles are disabled. Deliberately not free text |
| `took_ms` | int | How long the check ran, so one quietly becoming expensive is visible before it starts timing out |

A single check is bounded at **30 seconds**. Exceeding it produces
`not_assessed` / `undetermined` with the timeout named in the detail,
which is the honest answer: we do not know, because it did not finish. A
panic inside a check is recovered into the same shape rather than being
allowed to take down the agent that would report it.

## The catalogue

Three checks ship today on each of Linux and Windows. macOS has none yet
and publishes no posture rather than an empty result set.

### `disk-encryption` (category `encryption`)

The question people mean is about data at rest on a stolen machine, so
both platforms look at the **system volume specifically**. A machine with
an encrypted spare disk and a plaintext root is not an encrypted machine.

| Platform | `pass` | `fail` | `not_assessed` |
|---|---|---|---|
| Linux | The root filesystem sits on or under a `crypt` device (LUKS) | The root filesystem is on unencrypted storage | `lsblk` missing or unparseable, or no node claims to be mounted at `/`, which happens inside containers and on NFS or overlay roots |
| Windows | `Get-BitLockerVolume` reports the system drive protected | It reports the system drive unprotected | The cmdlet is unavailable, as on Home editions where BitLocker does not exist, or it reports a status we do not recognise |

The Linux check walks ancestors rather than the mounted node alone,
because with LUKS the mounted filesystem sits on a dm-crypt mapping whose
*parent* is the crypt layer. Checking only the node's own type finds
nothing on a correctly encrypted machine.

### `firewall` (category `firewall`)

| Platform | `pass` | `fail` | `not_assessed` |
|---|---|---|---|
| Linux | ufw active, firewalld running, or nftables with a non-empty ruleset | Any of those found and inactive, including an nftables ruleset that declares tables and chains but no rules | None of the three could be queried at all |
| Windows | Every network profile has the firewall enabled | Any profile has it disabled, named in the detail | The profiles could not be read, or none were reported |

Two asymmetries worth knowing. On Linux, "no firewall tool answered" is
`undetermined` rather than `fail`, because the machine might be running a
fourth thing or holding the ruleset behind a permission we lack, and
neither is a fact we established. On Windows it is **every** profile
rather than any: a machine with Domain enabled and Public disabled is
unprotected precisely where it matters, on the untrusted network it is
carried to.

### `antivirus` (category `malware`)

| Platform | Outcome |
|---|---|
| Linux | Always `not_assessed` / `not_applicable`. Resident antivirus is not part of the baseline on Linux, and failing a normal fleet for doing the normal thing is not a finding. A site that mandates one should have a check written for it |
| Windows | `pass` when a product is registered with Security Center and enabled, `fail` when one is registered and not enabled or when Security Center answered and listed nothing, `not_assessed` when the registration could not be read |

The Windows check reads Security Center rather than asking Defender about
itself. A third-party product registers with Security Center and Defender
stands down when it does, so asking Defender whether *it* is running
would report "off" on a machine that is perfectly well protected by
something else. Security Center answering with an empty list is a
verdict, because that genuinely means nothing is registered.

Windows checks shell out to PowerShell rather than going through WMI over
COM. COM is faster and is what the patch collector uses, and it needs a
locked OS thread, is stateful, and hangs. A posture check runs on a timer
on somebody's laptop and is worth none of that.

## On the wire

Posture publishes on `agents.{id}.inventory.posture` at startup and every
30 minutes, alongside the other inventory kinds, as one snapshot
containing every check:

```json
{
  "checks": [
    {
      "check": "antivirus",
      "category": "malware",
      "status": "not_assessed",
      "not_assessed_reason": "not_applicable",
      "detail": "resident antivirus is not part of the baseline on Linux"
    },
    {
      "check": "disk-encryption",
      "category": "encryption",
      "status": "pass",
      "detail": "the root filesystem is on an encrypted volume",
      "evidence": { "mechanism": "luks" },
      "took_ms": 12
    },
    {
      "check": "firewall",
      "category": "firewall",
      "status": "not_assessed",
      "not_assessed_reason": "undetermined",
      "detail": "no firewall tool could be queried on this machine",
      "took_ms": 4
    }
  ],
  "snapshot_hash": "..."
}
```

Results are sorted by check name before the snapshot is hashed. Without
that, goroutine ordering alone would produce a different hash on every
run and fill the history with churn that is not change.

The server records each result as its own bitemporal fact keyed
`check:<name>`, and exposes them through [`GET
/sync/posture`](/reference/sync-api/), one row per device and check. A
check absent from a device's rows never ran there, which is a third thing
again, distinct from both failing and being explicitly not assessed.

## Adding a check

A check is a type with `Name`, `Category` and `Run`, in its own file,
plus one line in that platform's list. The list is the whole registration
mechanism on purpose: a registry populated from `init()` would let a
check register itself at the cost of making the set of checks invisible,
where reading one explicit slice answers "what does this agent assess".

The interface's own contract is the rule from the top of this page.
`Run` returns a result and nothing else: there is no error return, so
"I could not determine this" has to be said as a status rather than
handed upwards as a failure for somebody else to interpret. Getting the
failure modes identical across checks is why every
one of them shells out through the same helper: getting it wrong once, in
one check, is how a missing binary on one platform turns into a fleet
reported as non-compliant.
