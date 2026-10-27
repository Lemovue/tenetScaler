# Build (if needed) and start the live demo dashboard. Ctrl+C stops it.
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
if (-not (Test-Path bin/tenantscaler.exe)) { & "$PSScriptRoot/build.ps1" }
& bin/tenantscaler.exe demo @args
