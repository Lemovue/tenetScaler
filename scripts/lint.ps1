# Lint: gofmt (must list nothing), go vet, JS syntax check, Python compile check.
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$failed = $false

$dirs = @('cmd', 'internal', 'test') | Where-Object { Test-Path $_ }
$unformatted = gofmt -l @dirs 2>&1
if ($unformatted) { Write-Output "gofmt: these files need formatting:"; $unformatted; $failed = $true }
else { Write-Output "gofmt: OK" }

go vet -p 4 ./...
if ($LASTEXITCODE -ne 0) { $failed = $true } else { Write-Output "go vet: OK" }

Get-ChildItem -Recurse -Filter *.js internal/web/static | ForEach-Object {
    node --check $_.FullName
    if ($LASTEXITCODE -ne 0) { $failed = $true }
}
if (-not $failed) { Write-Output "node --check: OK" }

python -m py_compile scripts/dev/resource_guard.py scripts/dev/heavy.py
if ($LASTEXITCODE -ne 0) { $failed = $true } else { Write-Output "py_compile: OK" }

if ($failed) { Write-Output "LINT FAILED"; exit 1 }
Write-Output "LINT OK"
