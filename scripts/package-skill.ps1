param(
  [switch]$UseUPX,
  [switch]$Sign,
  [string]$SignTool,
  [string]$CertificateThumbprint,
  [string]$CertificateFile,
  [string]$CertificatePassword,
  [string]$Description = 'Opsera',
  [string]$DescriptionUrl = 'https://github.com/tao-vin/opsera',
  [string]$TimestampUrl = 'http://timestamp.digicert.com',
  [string]$TrustedSigningDlib,
  [string]$TrustedSigningMetadata
)

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$skillDir = Join-Path $repoRoot 'skills\opsera'
$buildDir = Join-Path $repoRoot 'build'
$exePath = Join-Path $buildDir 'opsera.exe'
$releaseDir = 'D:\application\opsera'
$codexSkillDir = 'C:\Users\Administrator\.codex\skills\opsera'
$releaseExe = Join-Path $releaseDir 'opsera.exe'
$releaseRelayConfig = Join-Path $releaseDir 'relay-config.json'
$releaseIcon = Join-Path $releaseDir 'opsera.ico'
$zipPath = Join-Path $releaseDir 'opsera-skill.zip'
$codexSkillBinDir = Join-Path $codexSkillDir 'bin'
$codexSkillExe = Join-Path $codexSkillBinDir 'opsera.exe'
$stageRoot = Join-Path $env:TEMP ('opsera-skill-' + [guid]::NewGuid().ToString('N'))
$stageSkill = Join-Path $stageRoot 'opsera'
$buildTime = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$commit = 'unknown'
try {
  $commit = (& git rev-parse --short=12 HEAD 2>$null).Trim()
  if (-not $commit) {
    $commit = 'unknown'
  }
} catch {
  $commit = 'unknown'
}
$versionLdflags = "-X github.com/tao-vin/opsera/internal/version.BuildTime=$buildTime -X github.com/tao-vin/opsera/internal/version.Commit=$commit"

function Resolve-SignTool {
  param([string]$ExplicitPath)

  if ($ExplicitPath) {
    if (-not (Test-Path $ExplicitPath)) {
      throw "signtool.exe not found: $ExplicitPath"
    }
    return (Resolve-Path $ExplicitPath).Path
  }

  $command = Get-Command signtool.exe -ErrorAction SilentlyContinue
  if ($command) {
    return $command.Source
  }

  $candidates = @(
    "$env:ProgramFiles(x86)\Windows Kits\10\bin\*\x64\signtool.exe",
    "$env:ProgramFiles\Windows Kits\10\bin\*\x64\signtool.exe",
    "$env:ProgramFiles(x86)\Microsoft SDKs\ClickOnce\SignTool\signtool.exe"
  )
  $matches = foreach ($candidate in $candidates) {
    Get-Item $candidate -ErrorAction SilentlyContinue
  }
  $match = $matches | Sort-Object FullName -Descending | Select-Object -First 1
  if (-not $match) {
    throw 'signtool.exe not found. Install Windows SDK or pass -SignTool <path>.'
  }
  return $match.FullName
}

function Invoke-CodeSign {
  param([string]$Path)

  $tool = Resolve-SignTool $SignTool
  $args = @(
    'sign',
    '/v',
    '/fd', 'SHA256',
    '/tr', $TimestampUrl,
    '/td', 'SHA256',
    '/d', $Description,
    '/du', $DescriptionUrl
  )

  if ($TrustedSigningDlib -or $TrustedSigningMetadata) {
    if (-not $TrustedSigningDlib -or -not $TrustedSigningMetadata) {
      throw 'Trusted Signing requires both -TrustedSigningDlib and -TrustedSigningMetadata.'
    }
    $args += @('/dlib', $TrustedSigningDlib, '/dmdf', $TrustedSigningMetadata)
  } elseif ($CertificateFile) {
    $args += @('/f', $CertificateFile)
    if ($CertificatePassword) {
      $args += @('/p', $CertificatePassword)
    }
  } elseif ($CertificateThumbprint) {
    $args += @('/sha1', $CertificateThumbprint)
  } else {
    $args += @('/a')
  }

  $args += $Path
  & $tool @args
  if ($LASTEXITCODE -ne 0) {
    throw "signtool sign failed with exit code $LASTEXITCODE"
  }

  & $tool verify /pa /v $Path
  if ($LASTEXITCODE -ne 0) {
    throw "signtool verify failed with exit code $LASTEXITCODE"
  }
}

function Stop-TargetOpseraProcesses {
  param([string[]]$ExecutablePaths)

  $targets = @{}
  foreach ($path in $ExecutablePaths) {
    if ($path) {
      $targets[$path.ToLowerInvariant()] = $true
    }
  }
  Get-CimInstance Win32_Process |
    Where-Object { $_.Name -eq 'opsera.exe' -and $_.ExecutablePath -and $targets.ContainsKey($_.ExecutablePath.ToLowerInvariant()) } |
    ForEach-Object {
      Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }
  $deadline = (Get-Date).AddSeconds(5)
  do {
    Start-Sleep -Milliseconds 200
    $remaining = @(Get-CimInstance Win32_Process |
      Where-Object { $_.Name -eq 'opsera.exe' -and $_.ExecutablePath -and $targets.ContainsKey($_.ExecutablePath.ToLowerInvariant()) })
  } while ($remaining.Count -gt 0 -and (Get-Date) -lt $deadline)
}

New-Item -ItemType Directory -Force -Path $buildDir | Out-Null
New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null
New-Item -ItemType Directory -Force -Path $codexSkillBinDir | Out-Null

Push-Location $repoRoot
try {
  $go = (Get-Command go -ErrorAction SilentlyContinue).Source
  if (-not $go) {
    $go = 'C:\Program Files\Go\bin\go.exe'
  }
  if (-not (Test-Path $go)) {
    throw 'Go toolchain not found. Install Go or add go.exe to PATH.'
  }
  & $go build -trimpath -ldflags "-s -w $versionLdflags" -o $exePath .\cmd\opsera
  $upx = Get-Command upx.exe -ErrorAction SilentlyContinue
  if ($UseUPX) {
    if (-not $upx) {
      throw 'UPX requested but upx.exe was not found in PATH.'
    }
    & $upx.Source --best --lzma $exePath
  }
} finally {
  Pop-Location
}

if ($Sign) {
  Invoke-CodeSign $exePath
}

Stop-TargetOpseraProcesses @($releaseExe, $codexSkillExe)

Copy-Item $exePath $releaseExe -Force
Copy-Item (Join-Path $repoRoot 'relay-config.json') $releaseRelayConfig -Force
Copy-Item (Join-Path $repoRoot 'assets\opsera.ico') $releaseIcon -Force
Copy-Item (Join-Path $skillDir 'SKILL.md') (Join-Path $codexSkillDir 'SKILL.md') -Force
Copy-Item $exePath $codexSkillExe -Force
Copy-Item (Join-Path $repoRoot 'relay-config.json') (Join-Path $codexSkillBinDir 'relay-config.json') -Force
Copy-Item (Join-Path $repoRoot 'assets\opsera.ico') (Join-Path $codexSkillBinDir 'opsera.ico') -Force

New-Item -ItemType Directory -Force -Path $stageSkill | Out-Null
Copy-Item (Join-Path $skillDir 'SKILL.md') (Join-Path $stageSkill 'SKILL.md') -Force
New-Item -ItemType Directory -Force -Path (Join-Path $stageSkill 'bin') | Out-Null
Copy-Item $exePath (Join-Path $stageSkill 'bin\opsera.exe') -Force
Copy-Item (Join-Path $repoRoot 'relay-config.json') (Join-Path $stageSkill 'bin\relay-config.json') -Force
Copy-Item (Join-Path $repoRoot 'assets\opsera.ico') (Join-Path $stageSkill 'bin\opsera.ico') -Force

Remove-Item $zipPath -Force -ErrorAction SilentlyContinue
Compress-Archive -Path $stageSkill -DestinationPath $zipPath -Force
Remove-Item $stageRoot -Recurse -Force -ErrorAction SilentlyContinue

Write-Host "Executable: $releaseExe"
Write-Host "Codex skill executable: $codexSkillExe"
Write-Host "Relay config: $releaseRelayConfig"
Write-Host "Icon: $releaseIcon"
Write-Host "Skill package: $zipPath"
