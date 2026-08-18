---
title: Manage OS patches
description: Scan, approve, and deploy updates for apt, dnf, pacman, Windows Update, and macOS softwareupdate.
---

Patch management is a three-step loop: **scan** what each device could
install, **approve** what you want installed, **deploy** the approved set.
The steps are separate on purpose; nothing installs just because it
exists.

Supported package sources: `apt`, `dnf`, `pacman`, Windows Update, and
macOS `softwareupdate`.

## Scan

A patch scan is a job like any other, so it can run on demand, fleet-wide,
or on a [schedule](/guides/schedules/). The agent reports the pending set
as part of inventory, which means patch state gets the same
[bitemporal history](/concepts/bitemporal/) as everything else: you can
ask what was pending on a device last Tuesday, and separately what the
server believed was pending.

## Approve

The **Patches** view lists pending updates across the fleet with their
approval state. Approval is a record of intent, not an action; approving
ten updates changes nothing on any machine.

An assistant holding a `patches:write` scoped key can approve through the
MCP server too, and the tool is deliberately narrow: `approve_patches`
approves and nothing else. It cannot trigger installation, no matter how
it is asked.

## Deploy

Deploying sends `patch.install` jobs to the target devices, and delivery
is durable: offline machines install when they return. The result of a
patch job carries exactly what you want to know afterwards:

- which updates installed,
- which failed and why, per update,
- whether the device now wants a reboot.

Each installation also writes a `patch.installed` audit event, so the
patch history of a device is reconstructible from the audit trail
independent of the job log.

## After an incident

The bitemporal patch state is the piece most tools are missing. "Was the
fix for CVE-XXXX installed on web-01 when the incident started?" and "did
we *know* it was missing?" are different questions, and the second one is
usually the uncomfortable one. The
[device history guide](/guides/device-history/) shows how to ask both.
