$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$skillDir = Join-Path $repoRoot 'skills\opsera'
$binDir = Join-Path $skillDir 'bin'
$exePath = Join-Path $repoRoot 'opsera.exe'
$zipPath = Join-Path $repoRoot 'dist\opsera-skill.zip'

if (-not (Test-Path $exePath)) {
  throw "Missing $exePath. Build opsera.exe first."
}

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $zipPath) | Out-Null
Copy-Item $exePath (Join-Path $binDir 'opsera.exe') -Force

Remove-Item $zipPath -Force -ErrorAction SilentlyContinue
Compress-Archive -Path $skillDir -DestinationPath $zipPath -Force

Write-Host "Skill package: $zipPath"
