# Build the single tenantscaler binary into bin/.
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
New-Item -ItemType Directory -Force bin | Out-Null
$commit = (git rev-parse --short HEAD 2>$null)
if (-not $commit) { $commit = 'unknown' }
go build -p 4 -ldflags "-X main.version=0.1.0+$commit" -o bin/tenantscaler.exe ./cmd/tenantscaler
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Output "built bin/tenantscaler.exe"
