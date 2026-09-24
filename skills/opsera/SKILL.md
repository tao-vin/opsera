---
name: opsera
description: Use the bundled Opsera executable for agent SSH operations through saved Opsera servers or live DASUSM/USMSSO-launched Xshell sessions. Use when an agent needs to run commands or transfer files after a VPN/SSO login, especially when XshellCore.exe is already connected. Prefer `command run --sso` for DASUSM/USMSSO/XshellCore sessions; use .xsh only as a legacy fallback.
---

# Opsera

Opsera is the agent-facing SSH tool. It gives agents a CLI/API path instead of driving Xshell UI.

Source and downloads:

- GitHub: https://github.com/tao-vin/opsera
- Releases / latest exe: https://github.com/tao-vin/opsera/releases

## Resolve Executable

Use the bundled exe from this skill. Do not use `ssh-remote-ops`, Xshell automation, or external install paths.

```powershell
$opsera = Join-Path $env:CODEX_HOME "skills\opsera\bin\opsera.exe"
if (-not $env:CODEX_HOME -or -not (Test-Path $opsera)) {
  $opsera = "C:\Users\Administrator\.codex\skills\opsera\bin\opsera.exe"
}
```

## Choose the Correct Connection Mode

There are exactly four common Opsera connection modes. Identify the mode before running commands:

1. Direct saved server: the target Linux server is saved in the current machine's Opsera config. Use `--server`.
2. Bastion through Xshell: the current machine has an SSO/DASUSM-launched Xshell/XshellCore session. Use `--sso`.
3. Bastion through already-open Opsera: the current machine has a desktop Opsera window already opened by the bastion workflow. The launched target should appear in the left server list and become the active terminal tab. Use `session list` and `session run`.
4. Remote Windows through relay: another Windows machine is running Opsera and the user provides `Code` and `Password`. Use `relay`.

Do not mix these modes:

- `--server <ip>` only works for mode 1 and only if `opsera server list` on the current machine shows that server.
- `--sso` only works for mode 2 and depends on live Xshell/XshellCore token capture.
- `session run` only works for mode 3 and depends on the desktop Opsera API on the same machine at `127.0.0.1:18741`.
- `relay` only works for mode 4 and needs the remote Opsera device `Code` and `Password`.

Fast decision flow:

```powershell
# Mode 1: current machine saved server
& $opsera server list
& $opsera command run --server <name-or-host-from-list> "hostname && whoami"

# Mode 2: current machine Xshell/SSO
& $opsera sso status
& $opsera sso attach
& $opsera xshell keepalive
& $opsera command run --sso "hostname && whoami"

# Mode 3: current machine already-open Opsera desktop session
& $opsera session list
& $opsera session run "hostname; whoami; pwd"

# Mode 4: another Windows machine through relay
& $opsera relay status <code> <password> opsera.deepcode.art
& $opsera relay exec <code> <password> "hostname; whoami; Get-Location" opsera.deepcode.art
```

## Primary: DASUSM/USMSSO Attach

When the user has already launched the VPN/SSO entry and Xshell is open, run commands with `--sso`.

```powershell
$opsera = Join-Path $env:CODEX_HOME "skills\opsera\bin\opsera.exe"
if (-not $env:CODEX_HOME -or -not (Test-Path $opsera)) {
  $opsera = "C:\Users\Administrator\.codex\skills\opsera\bin\opsera.exe"
}
& $opsera sso status
& $opsera sso attach
& $opsera xshell keepalive
& $opsera command run --sso "hostname && whoami"
```

`sso attach` starts a local SSO agent on `127.0.0.1:18742`, opens one SSH connection while the Xshell token is fresh, and keeps that SSH connection alive. Later `command run --sso`, `file upload --sso`, `file download --sso`, and `file upload-large --sso` reuse the agent connection first; only if the agent is unavailable do they fall back to a one-shot Xshell URL connection. The agent also watches for new Xshell process command lines and auto-attaches when the Xshell URL changes.

The SSO agent attaches to live Xshell process command lines and extracts `ssh://user:password@host:port` URLs. It tries visible live URLs and keeps the first SSH connection that really authenticates. Failed SSH URLs are cooled down during the agent lifetime so an expired token is not retried repeatedly. Keep the official Xshell window running until `sso attach` has succeeded. After attach, the SSH connection can usually survive Xshell token expiry as long as the server does not force-close existing sessions.

Do not treat an online Xshell window as proof that Opsera can connect. Always check `sso status`:

- `xshellWindow: true` means the official Xshell window is visible.
- `xshellUrlPresent: true` means an SSH URL exists in Xshell/XshellCore process command lines.
- `opseraConnected: true` is the only state that means agents can use `--sso`.

If `xshellWindow` is true but `opseraConnected` is false, the visible Xshell terminal may still be logged in while the reusable SSH URL token has expired. Reopen the SSH session from SSO and immediately run `& $opsera sso attach`.

The SSO agent also keeps the DASUSM browser login warm by default. It looks for Chrome/Edge windows whose title matches `主机运维`, `DASUSM`, `Plate`, or `100.200.128.13`, refreshes one matching page every 18 minutes, and then tries to restore the previous foreground window. Disable this before agent startup with `--no-browser-keepalive` or `OPSERA_DASUSM_BROWSER_KEEPALIVE=0`; adjust the interval with `OPSERA_DASUSM_BROWSER_KEEPALIVE_INTERVAL_MINUTES`.

Use `--core-only` only for a known environment where `XshellCore.exe` URLs are confirmed to authenticate. Do not use it as the default; some DASUSM launches put the reusable token only on `Xshell.exe`.

```powershell
& $opsera sso attach --core-only
& $opsera command run --sso "hostname && whoami"
```

For VPN/SSO/Xshell mode, Xshell window keepalive is required. In this environment, only sending a real `Enter` in the visible official Xshell terminal has proven reliable for preventing the VPN-launched terminal from going idle. After `sso attach`, always run `opsera xshell keepalive` before doing longer work.

Do not enable `--window-keepalive` or `OPSERA_XSHELL_WINDOW_KEEPALIVE`. Use the explicit `opsera xshell keepalive` command instead, because it verifies Xshell is foreground, sends a real `Enter`, and restores the previous window.

Required Xshell keepalive command for VPN/SSO/Xshell work:

```powershell
& $opsera xshell keepalive
```

This command starts a hidden background keepalive worker and returns quickly. The worker briefly activates the Xshell window every 3 minutes by default, verifies Xshell actually became the foreground window, clicks the terminal area, sends a real `Enter`, then restores the previous window. It writes a small status log to `%TEMP%\opsera-xshell-keepalive.log`.

The default interval is 180 seconds. To override it, set `OPSERA_XSHELL_KEEPALIVE_INTERVAL_SECONDS` before starting keepalive:

```powershell
$env:OPSERA_XSHELL_KEEPALIVE_INTERVAL_SECONDS = "60"
& $opsera xshell keepalive
```

Use 60 seconds for unstable VPN sessions, 180 seconds for the normal tested default. Check the active interval and recent activity with:

```powershell
& $opsera xshell keepalive status
Get-Content "$env:TEMP\opsera-xshell-keepalive.log" -Tail 20
```

If it fails, do not spend time trying random SSH/Xshell methods. First inspect whether XshellCore is alive:

```powershell
Get-CimInstance Win32_Process |
  Where-Object { $_.Name -match 'Xshell|XshellCore|cusmsso|usmsso' } |
  ForEach-Object { '---'; $_.Name; $_.ExecutablePath; $_.CommandLine }
```

If no `Xshell.exe` or `XshellCore.exe` command line contains `ssh://`, ask the user to launch the SSO/VPN entry again and retry immediately.

On Windows systems where `Win32_Process.CommandLine` is access-denied from the normal agent process, rerun the Opsera command from an elevated/approved shell. Do not fall back to the DASUSM log `SSOToken`; that token is not the SSH password.

## Saved Opsera Server

Use `--server` only for servers saved on the current machine. `--server <name|host|id>` does not mean "connect to any reachable IP" and it does not search another computer's desktop Opsera session. If `opsera server list` does not show the target, `--server <ip>` will fail with `server not found`; do not keep retrying it.

For servers saved inside Opsera, first search the saved server notes/projects when the user names a product, project, domain, app, or business system but does not know the server name. The desktop app has a `Notes / projects` field on each server; it should list the projects hosted there, important paths, ports, domains, and service names.

```powershell
& $opsera server search "cpcn"
& $opsera server search "deepcode.art"
& $opsera server list
```

Pick the matching `name`, `id`, or `host`, then use `--server`:

```powershell
& $opsera command run --server fei "hostname && whoami"
```

If `server search` returns multiple candidates, prefer the one whose notes explicitly mention the target project/domain. If no server note matches, list all servers and inspect likely hosts with lightweight commands only, such as `hostname`, `pwd`, `ls /data/application /opt`, and `ss -lntp`.

## Remote Opsera / Relay Device

When the user provides `Code` and `Password`, or says the target is another machine, a remote desktop/relay machine, a machine opened from the bastion host, or an Opsera window on someone else's computer, use relay commands. Do not use `--server <ip>` unless that server is also saved in the current machine's `opsera server list`.

```powershell
& $opsera relay status <code> <password> opsera.deepcode.art
& $opsera relay exec <code> <password> "hostname; whoami; Get-Location" opsera.deepcode.art
& $opsera relay upload <code> <password> "D:\local\file.txt" "D:\remote\file.txt" opsera.deepcode.art
```

Interpretation rules:

- `relay status` online means the remote Opsera device is reachable through the relay.
- `server not found: <ip>` means the current machine has no saved local server entry for that IP. It is not an SSH authentication error and not proof that the remote machine is offline.
- If a user says "I opened Opsera from the bastion host on another machine", the current agent cannot read that other machine's local server list. Use the relay `Code` and `Password`, or run Opsera locally on that other machine.
- If you need to operate a Linux SSH server behind that other machine, first use `relay exec` on the remote Windows machine to inspect what that machine can see, or ask the user for a saved local server name that exists on the current machine.

## Already Open Desktop Opsera Session

When the user says the bastion host has already opened Opsera, or the target is "the currently opened Opsera" on this same Windows desktop, do not use `--server`, relay, or Xshell first. Query the local desktop Opsera API on that machine:

```powershell
Invoke-RestMethod http://127.0.0.1:18741/health
Invoke-RestMethod http://127.0.0.1:18741/sessions | ConvertTo-Json -Depth 5
& $opsera session diagnose
```

If `/sessions` returns a current session, send the command to that open Opsera session:

```powershell
& $opsera session list
& $opsera session run "hostname; whoami; pwd"
```

To test whether a bastion/VPN launch can be captured, do not guess with `--server <ip>`. Simulate the exact launch path and require the result to show `desktopAPI=true`, `serverPresent=true`, and `sessionPresent=true`:

```powershell
& $opsera session simulate-launch opsera://192.168.70.10
& $opsera session diagnose 192.168.70.10
```

Diagnostic interpretation:

- `desktopAPI=false`: desktop Opsera is not running on this same Windows machine, so this agent cannot see the window.
- `no bastion launch target`: Opsera was opened, but no usable launch argument was received or logged.
- `serverPresent=false`: the target was parsed but did not enter the left server list.
- `sessionPresent=false`: the target entered the list but the right-side session/terminal was not opened.
- `status=ok`: the bastion target was captured, listed, and active; use `session run`.

The equivalent HTTP endpoint is `POST http://127.0.0.1:18741/session/run` with JSON body `{"command":"hostname; whoami; pwd"}`. Do not use `/session/command` for agent automation; it is only a queued UI command path and may not return execution output.

Interpretation rules:

- This mode is about the Opsera desktop window already open on the same machine as the agent.
- The bastion-launched target should appear in the left server list as a captured VPN/bastion target and should be selected by default with a terminal tab open on the right.
- It has nothing to do with `opsera server list`; a current desktop session may exist even when no saved server entry matches the IP.
- It has nothing to do with relay unless the open Opsera is on another computer.
- It has nothing to do with Xshell unless Opsera was launched from an Xshell/SSO workflow.
- If `127.0.0.1:18741` is unreachable, the current machine does not have the Opsera desktop API running. Do not claim the open Opsera on a different computer is accessible from here; run the agent on that computer or use relay.

## Legacy Xshell Session File

Use `.xsh` only when there is no DASUSM/USMSSO live XshellCore session:

```powershell
& $opsera command run --xsh "<path.xsh>" "hostname && whoami"
```

## Upload

For small files through live DASUSM/USMSSO Xshell sessions:

```powershell
& $opsera file upload --sso "D:\local\file.txt" "/root/file.txt"
```

Do not use `file upload --sso` for large jars/APKs. Use `file upload-large --sso` so the command blocks until the final file is merged and verified.

For saved Opsera servers:

```powershell
& $opsera file upload --server fei "D:\local\file.txt" "/root/file.txt"
```

For large files through saved Opsera servers, use `upload-large --server` instead of plain `file upload`. It uploads resumable parts, merges remotely, verifies sha256, and only returns success after the final remote file matches:

```powershell
& $opsera file upload-large --server fei --chunk-mb 128 "D:\local\app.jar" "/data/application/app/app.jar.new"
```

For legacy `.xsh` sessions:

```powershell
& $opsera file upload --xsh "<path.xsh>" "D:\local\file.txt" "/root/file.txt"
```

## Large Upload

Use this for large files. It uploads resumable chunks, keeps SSH alive, merges remotely, and verifies sha256.

```powershell
& $opsera sso status
& $opsera sso attach
& $opsera file upload-large --server fei --chunk-mb 128 "D:\local\big.dat" "/root/big.dat"
& $opsera file upload-large --sso --chunk-mb 512 "D:\local\big.dat" "/root/big.dat"
& $opsera file upload-large --xsh "<path.xsh>" --chunk-mb 512 "D:\local\big.dat" "/root/big.dat"
```

`upload-large --sso` must not be treated as successful unless the agent returns the final remote size and sha256 matching the local file. If it returns early, leaves only `.parts/part-*.tmp`, or does not create the final remote file, treat that as a failed upload and rerun after reconnecting SSO.

## Download

```powershell
& $opsera file download --sso "/root/file.txt" "D:\local\file.txt"
& $opsera file download --xsh "<path.xsh>" "/root/file.txt" "D:\local\file.txt"
```

## Rules

- Prefer `command run --sso` for DASUSM/USMSSO-launched Xshell sessions.
- Use the bundled `bin\opsera.exe`; do not depend on external install directories.
- Use CLI mode, not HTTP, for agent operations.
- Do not call Xshell directly and do not use `ssh-remote-ops`.
- Do not expose passwords or print full XshellCore URLs unless debugging connection setup.
- If `--sso` fails because no live Xshell URL is present or the agent cannot reconnect, ask the user to launch the SSO/VPN entry again and rerun `sso attach` immediately.
