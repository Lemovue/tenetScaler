# Run all tests: unit tests (parallel packages capped at 4), then the
# end-to-end smoke tests against the running app (single worker).
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
$env:GOMAXPROCS = '8'

$unit = go list ./... | Where-Object { $_ -notlike '*/test/e2e*' }
go test -p 4 -count=1 -timeout 600s $unit
if ($LASTEXITCODE -ne 0) { Write-Output "UNIT TESTS FAILED"; exit 1 }

if (Test-Path test/e2e) {
    go test -p 1 -count=1 -timeout 900s -v ./test/e2e/...
    if ($LASTEXITCODE -ne 0) { Write-Output "E2E TESTS FAILED"; exit 1 }
}
Write-Output "ALL TESTS PASSED"
