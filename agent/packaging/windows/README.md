# Windows packaging

Two artifacts per release:

- `everwas-agent_<version>_windows_amd64.msi` — for anything unattended: GPO,
  SCCM/Intune, another RMM's software push, a scripted rebuild.
- `everwas-agent_<version>_windows_<arch>.zip` — the bare exe, for arm64 and
  for anyone who would rather drive `everwas-agent.exe install` themselves.

Both carry an Authenticode signature. A release that reaches the release page
unsigned is a bug in the pipeline, not a variant to work around: see
`docs/windows-code-signing.md`.

## Installing with the MSI

Unattended, enrolling as it installs:

```
msiexec /i everwas-agent_2026.08.17_windows_amd64.msi /qn ^
  SERVER=https://rmm.example.com TOKEN=your-enrollment-token
```

Install now, enroll later (the service starts and idles):

```
msiexec /i everwas-agent_2026.08.17_windows_amd64.msi /qn
"C:\Program Files\Everwas\Agent\everwas-agent.exe" enroll --server URL --token TOKEN
```

Properties:

| Property | Meaning |
| --- | --- |
| `SERVER` | server base URL. Must be given together with `TOKEN`. |
| `TOKEN` | one-time enrollment token. |
| `PURGE=1` | on **uninstall** only: also delete `C:\ProgramData\Everwas\Agent`. |

A bad or expired token fails the install and rolls it back. That is
deliberate. An MSI that returns success while leaving an agent that never
enrolled is invisible to whatever pushed it, and the way that surfaces is
someone noticing weeks later that a rollout produced no devices.

The token is in `MsiHiddenProperties` and the enrolling custom action is
`HideTarget`, so it does not reach the MSI log. It is still visible in the
process list for the second or two the enrollment takes. Mint short-lived
one-time tokens and treat any token that has been through a deployment tool
as spent.

For a log when something goes wrong: `msiexec /i ... /qn /l*v install.log`.

### Group Policy

The MSI is a per-machine package with no UI sequence, so it works as a
Computer Configuration → Software Installation assignment. GPO cannot pass
properties, so either use a transform (`.mst`) carrying `SERVER` and `TOKEN`,
or assign the package without them and enroll from a startup script. The
second is usually better: one token per host is the point of a one-time
token, and a transform pins one token for every host the policy touches.

## What the installer actually does

Files go to `C:\Program Files\Everwas\Agent\`, and
`HKLM\SOFTWARE\Everwas\Agent` gets `Version` (the CalVer string) and
`InstallPath`.

The service is registered by running the agent's own `install` subcommand,
not by MSI `ServiceInstall`/`ServiceControl` rows. That is not a shortcut.
`svc.Install` sets recovery actions, sets
`SERVICE_CONFIG_FAILURE_ACTIONS_FLAG` so a non-crash non-zero exit still
counts as a failure worth restarting, and updates an existing service in
place so its SID and any operator ACL edits survive an upgrade. MSI's service
tables cannot express the second one, and without it a self-update leaves the
host with no agent process until the next reboot, because the agent exits
cleanly-but-non-zero on purpose in order to be restarted.

## Uninstalling, and what happens to the identity

```
msiexec /x everwas-agent_2026.08.17_windows_amd64.msi /qn
```

stops the service, deregisters it, and removes the binary. It does **not**
touch `C:\ProgramData\Everwas\Agent`, which holds `agent.json`: the agent id
and the NATS secret.

That is the deliberate choice, and it has a cost in each direction.

Keeping the identity means an uninstall revokes nothing. The server still
trusts that agent id, and a later reinstall on the same host silently
re-adopts the old enrollment rather than appearing as a new device. If the
host is being decommissioned or handed to someone else, revoke it on the
server; an installer cannot do that for you, and pretending otherwise would
be worse than saying so here.

Removing the identity by default would cost more. The uninstall half of every
major upgrade would destroy it, so each version bump would need a fresh
enrollment token for every host. Reinstall-to-fix-it, which is the common
reason anyone runs an uninstall at all, would need a token that whoever is
doing it at 2am does not have. And each cycle would leave a stale device
record on the server with nothing to correlate the returning host against.

When you do mean it:

```
msiexec /x everwas-agent_2026.08.17_windows_amd64.msi /qn PURGE=1
```

`PURGE=1` is ignored during a major upgrade (`UPGRADINGPRODUCTCODE` is set),
so it cannot fire during the removal half of a version bump.

## Upgrades

Installing a newer MSI removes the old product first
(`RemoveExistingProducts` right after `InstallInitialize`), which stops and
deregisters the service before any file is touched. Scheduling it later would
try to overwrite `everwas-agent.exe` while the SCM still has it open, and
Windows answers that with a reboot prompt. The service is down for the length
of the install.

Downgrades are refused with a message rather than half-applied.

`UpgradeCode` is `{C715EB57-9DA9-4A77-AD9E-FA4A78DE65F0}` and is permanent.
Changing it orphans every installed agent: MSI would stop recognising the old
package as the same product, and an upgrade would become a second
Add/Remove entry fighting over the same directory.

The MSI `ProductVersion` is not the CalVer string. MSI caps the first version
field at 255 and ignores the fourth entirely, so `2026.08.17.1` becomes
`26.8.1701` (`YY.M.DDPP`). `build-msi.sh` explains the mapping; the CalVer
string a human recognises is in the registry and in the package comments.

## Installing from the zip

```
everwas-agent.exe install --server https://rmm.example.com --token YOUR_TOKEN
```

from an **Administrator** prompt. Same install location, same service
configuration; the MSI is a wrapper around this command, not a second
implementation. `everwas-agent.exe uninstall [--purge]` is the reverse.

Verify what you downloaded first. From the same directory as the zip and the
release's `SHA256SUMS`:

```powershell
$want = (Select-String -Path SHA256SUMS -Pattern 'windows_amd64.zip').Line.Split(' ')[0]
$got  = (Get-FileHash .\everwas-agent_<version>_windows_amd64.zip -Algorithm SHA256).Hash.ToLower()
if ($want -ne $got) { throw "checksum mismatch" }

Get-AuthenticodeSignature .\everwas-agent.exe | Format-List Status, SignerCertificate
```

`Status` must be `Valid`. `NotSigned` means you have an artifact that never
should have been published.

## Checking on it

```
sc.exe query everwas-agent
sc.exe qfailure everwas-agent
```

The agent logs JSON to stderr, which the SCM discards. Until an Event Log
sink lands, run `everwas-agent.exe run` from an elevated prompt to watch it
directly.

## Self-update behaviour

A running `.exe` cannot be overwritten but it can be renamed, which is how the
in-place swap works: the current binary is renamed to `everwas-agent.old.exe`,
the verified new binary is moved in, and the process exits so the SCM restarts
it. `SetRecoveryActionsOnNonCrashFailures` is enabled at install time
precisely so that clean exit counts as a restart trigger.

If a scanner or backup agent holds a handle and the rename is refused, the
updater falls back to spawning the staged binary with a hidden
`update-finalize --pid N --target PATH --staged PATH --state-dir DIR --version V`
subcommand. That helper waits for the old process to exit, performs the swap
from outside, and starts the service again.

The handoff is reported as **finalizing**, which is not the same as applied:
the host is still running the old binary until the helper says otherwise. The
helper writes its outcome to `update-state.json` on every exit path, including
the one where it gave up waiting for the old process, so a finalize that never
happened shows up as `finalize_failed` rather than as silence.

A self-update swaps the binary underneath the MSI. Add/Remove Programs keeps
showing the version that was installed, and the registry `Version` value with
it; `everwas-agent.exe status` and the server are the truth. Installing a
newer MSI over a self-updated agent still works, because the MSI's version
comparison is against the installed *package*, not against the file on disk.

## Rollback on Windows

There is no `ExecStartPre` in the SCM. The recovery actions are three plain
restarts (5s, 30s, 60s) and nothing else; the "restore the previous binary"
recovery command that used to sit in the third slot was removed, because it
fired on any three failures in a day and moved an arbitrarily old backup over
the current binary with no record anywhere. See the comment in
`internal/svc/service_windows.go`.

## Building the MSI

```
./packaging/windows/build-msi.sh \
  --exe dist/everwas-agent_windows_amd64_v1/everwas-agent.exe \
  --arch amd64 --version 2026.08.17 --out dist
```

Needs `msitools` (`wixl`, `msiinfo`, `msibuild`). In a release it is called
from the GoReleaser Windows post-build hook, so the exe it wraps is the same
signed file that goes into the zip.

`EVERWAS_ALLOW_UNSIGNED=1` skips the signature requirement for a local build.
That artifact must not leave the workstation: it will warn about an unknown
publisher on every host that sees it.

### Why wixl and not WiX

WiX v4, v5 and v6 all install cleanly as dotnet tools on Linux and then
refuse to do anything useful: every invocation prints

```
warning WIX0000: The WiX Toolset only supports Windows ... All behavior after
this point is undefined.
```

and a minimal package fails with `WIX0389: The Directory/@Name attribute's
value, 'Agent', is not a relative path`, because the name validation runs
through .NET path APIs that behave differently off Windows
(wixtoolset/issues#7154, open since v4 rc1). Tested with 4.0.6, 5.0.2 and
6.0.2; all three fail identically.

msitools' `wixl` builds the same package natively on Linux, which keeps the
whole release on one runner. The cost is that wixl implements a subset of the
WiX v3 schema and is silent about the rest: `Property/@Secure`,
`CustomAction/@HideTarget` and `Before`/`After` sequencing are all parsed and
discarded without a warning. Each of those produces a package that builds,
installs, and is wrong. So `build-msi.sh` patches the two attributes into the
built database with `msibuild`, and `check-msi.sh` asserts the shape of the
result: sequence numbers inside the InstallInitialize/InstallFinalize window,
the custom action type bits, `SecureCustomProperties`, the 64-bit component
flag. CI builds the MSI and runs those checks on every change.

The alternative is a `windows-latest` job running real WiX, which is the
supported configuration and would let the `.wxs` use v4 syntax. It costs a
second runner, an artifact handoff, and Authenticode signing split across two
platforms. Worth revisiting if wixl's subset ever stops being enough.

`wixl` cannot target arm64 (`arch of type 'arm64' is not supported`), so there
is no arm64 MSI. Windows 11 on ARM runs the x64 agent and the x64 MSI under
emulation, and the zip is there for anyone who wants the native build.
