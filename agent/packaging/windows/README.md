# Windows packaging

The Windows release ships as a plain `.exe` inside a zip. There is no MSI yet.

## Installing

Download `openrmm-agent_<version>_windows_amd64.zip` from the release page,
unzip it, then from an **Administrator** PowerShell or cmd prompt:

```
openrmm-agent.exe install --server https://rmm.example.com --token YOUR_TOKEN
```

That copies the binary to `C:\Program Files\OpenRMM\Agent\openrmm-agent.exe`,
enrolls the host, registers the `openrmm-agent` service with the Service
Control Manager (start type automatic, restart on failure at 5s and 30s, then
a recovery command at 60s that restores the previous binary), and starts it.

Verify the checksum first. From the same directory as the downloaded zip and
the release's `SHA256SUMS`:

```powershell
$want = (Select-String -Path SHA256SUMS -Pattern 'windows_amd64.zip').Line.Split(' ')[0]
$got  = (Get-FileHash .\openrmm-agent_<version>_windows_amd64.zip -Algorithm SHA256).Hash.ToLower()
if ($want -ne $got) { throw "checksum mismatch" }
```

## Uninstalling

```
openrmm-agent.exe uninstall
```

Add `--purge` to also delete `C:\ProgramData\OpenRMM\Agent` (the agent
identity) and the installed binary. Without `--purge` the identity survives,
so a reinstall reconnects as the same agent.

## Checking on it

```
sc.exe query openrmm-agent
sc.exe qfailure openrmm-agent
```

The agent logs JSON to stderr, which the SCM discards. Until an Event Log
sink lands, run `openrmm-agent.exe run` from an elevated prompt to watch it
directly.

## Self-update behaviour

A running `.exe` cannot be overwritten but it can be renamed, which is how the
in-place swap works: the current binary is renamed to `openrmm-agent.old.exe`,
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

## Rollback on Windows

There is no `ExecStartPre` in the SCM, so the equivalent of the unix guard is
the third recovery action:

```
cmd.exe /C move /Y "...\openrmm-agent.old.exe" "...\openrmm-agent.exe" && sc.exe start openrmm-agent
```

Two failed starts get restarts; the third runs that command, which puts the
previous binary back. It runs from the SCM rather than from the agent, which
is the point: it works when the new build cannot execute at all. Inspect it
with `sc.exe qfailure openrmm-agent`.

The previous binary is NOT deleted when a build is declared healthy. One spare
generation is the only recovery path on a host whose agent is the thing that
would otherwise have to fetch a fix.

## Deferred: MSI / WiX

An MSI would give Add/Remove Programs integration, Group Policy deployment,
per-machine upgrade codes and proper rollback. It is deferred because it needs
a WiX toolset build step, an upgrade GUID policy that has to be right the
first time (an MSI's `UpgradeCode` is permanent), and code signing to be
useful in the environments that would want it. The exe based install covers
scripted and RMM-pushed deployment today.

When it lands, the pieces will be:

- `Product.wxs` with a `ServiceInstall` and `ServiceControl` element rather
  than a custom action, so Windows Installer owns the service lifecycle
- a `MajorUpgrade` element with `Schedule="afterInstallInitialize"`
- properties for `SERVER` and `TOKEN` so `msiexec /i openrmm-agent.msi
  SERVER=... TOKEN=... /qn` enrolls unattended
- an Authenticode signature on both the exe and the msi
