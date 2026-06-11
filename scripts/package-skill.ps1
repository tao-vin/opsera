$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$skillDir = Join-Path $repoRoot 'skills\opsera'
$buildDir = Join-Path $repoRoot 'build'
$exePath = Join-Path $buildDir 'opsera.exe'
$releaseDir = 'D:\release'
$releaseExe = Join-Path $releaseDir 'opsera.exe'
$zipPath = 'D:\release\opsera-skill.zip'
$stageRoot = Join-Path $env:TEMP ('opsera-skill-' + [guid]::NewGuid().ToString('N'))
$stageSkill = Join-Path $stageRoot 'opsera'

New-Item -ItemType Directory -Force -Path $buildDir | Out-Null
New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null

Push-Location $repoRoot
try {
  $go = (Get-Command go -ErrorAction SilentlyContinue).Source
  if (-not $go) {
    $go = 'C:\Program Files\Go\bin\go.exe'
  }
  if (-not (Test-Path $go)) {
    throw 'Go toolchain not found. Install Go or add go.exe to PATH.'
  }
  & $go build -ldflags "-H windowsgui" -o $exePath .\cmd\opsera
} finally {
  Pop-Location
}

Copy-Item $exePath $releaseExe -Force

New-Item -ItemType Directory -Force -Path $stageSkill | Out-Null
Copy-Item (Join-Path $skillDir 'SKILL.md') (Join-Path $stageSkill 'SKILL.md') -Force
New-Item -ItemType Directory -Force -Path (Join-Path $stageSkill 'bin') | Out-Null
Copy-Item $exePath (Join-Path $stageSkill 'bin\opsera.exe') -Force

Remove-Item $zipPath -Force -ErrorAction SilentlyContinue
Compress-Archive -Path $stageSkill -DestinationPath $zipPath -Force
Remove-Item $stageRoot -Recurse -Force -ErrorAction SilentlyContinue

Write-Host "Executable: $releaseExe"
Write-Host "Skill package: $zipPath"
