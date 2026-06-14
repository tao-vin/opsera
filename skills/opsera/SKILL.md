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

## Primary: DASUSM/USMSSO XshellCore Attach

When the user has already launched the VPN/SSO entry and Xshell is open, run commands with `--sso`.

```powershell
& $opsera sso attach
& $opsera command run --sso "hostname && whoami"
```

`sso attach` starts a local SSO agent on `127.0.0.1:18742`, opens one SSH connection while the Xshell token is fresh, and keeps that SSH connection alive. Later `command run --sso`, `file upload --sso`, `file download --sso`, and `file upload-large --sso` reuse the agent connection first; only if the agent is unavailable do they fall back to a one-shot Xshell URL connection. The agent also watches for new Xshell process command lines and auto-attaches when the Xshell URL changes.

The SSO agent attaches to live Xshell process command lines and extracts `ssh://user:password@host:port` URLs. It prefers the newest official `XshellCore.exe -url ...` value, then falls back to `Xshell.exe -url ...`. Failed SSH URLs are cooled down during the agent lifetime so an expired token is not retried repeatedly. Keep the official Xshell window running until `sso attach` has succeeded. After attach, the SSH connection can usually survive Xshell token expiry as long as the server does not force-close existing sessions.

If a stale `Xshell.exe` launcher URL keeps winning in a specific environment, run the agent in Core-only mode. This keeps all existing behavior but ignores `Xshell.exe` process URLs and uses only official `XshellCore.exe` URLs:

```powershell
$env:OPSERA_SSO_CORE_ONLY = "1"
& $opsera sso attach --core-only
& $opsera command run --sso "hostname && whoami"
```

If the official Xshell window disconnects when idle, enable window keepalive. This brings the visible Xshell/XshellCore window forward once per minute and sends `ls` followed by Enter, so the bastion sees normal terminal activity:

```powershell
$env:OPSERA_XSHELL_WINDOW_KEEPALIVE = "1"
& $opsera sso attach --window-keepalive
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

For servers saved inside Opsera, use `--server`:

```powershell
& $opsera command run --server fei "hostname && whoami"
```

## Legacy Xshell Session File

Use `.xsh` only when there is no DASUSM/USMSSO live XshellCore session:

```powershell
& $opsera command run --xsh "<path.xsh>" "hostname && whoami"
```

## Upload

For live DASUSM/USMSSO Xshell sessions:

```powershell
& $opsera file upload --sso "D:\local\file.txt" "/root/file.txt"
```

For saved Opsera servers:

```powershell
& $opsera file upload --server fei "D:\local\file.txt" "/root/file.txt"
```

For legacy `.xsh` sessions:

```powershell
& $opsera file upload --xsh "<path.xsh>" "D:\local\file.txt" "/root/file.txt"
```

## Large Upload

Use this for large files. It uploads resumable chunks, keeps SSH alive, merges remotely, and verifies sha256.

```powershell
& $opsera file upload-large --sso --chunk-mb 512 "D:\local\big.dat" "/root/big.dat"
& $opsera file upload-large --xsh "<path.xsh>" --chunk-mb 512 "D:\local\big.dat" "/root/big.dat"
```

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
