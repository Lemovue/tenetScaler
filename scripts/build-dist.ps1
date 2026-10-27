# Cross-compile the tenantscaler binary (it includes `loadgen` for teammates'
# laptops) for windows/amd64, darwin/arm64 and linux/amd64 into dist/.
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)
New-Item -ItemType Directory -Force dist | Out-Null
$commit = (git rev-parse --short HEAD 2>$null)
if (-not $commit) { $commit = 'unknown' }
$env:CGO_ENABLED = '0'
foreach ($t in @(@('windows', 'amd64', '.exe'), @('darwin', 'arm64', ''), @('linux', 'amd64', ''))) {
    $env:GOOS, $env:GOARCH = $t[0], $t[1]
    $out = "dist/tenantscaler-$($t[0])-$($t[1])$($t[2])"
    go build -p 4 -trimpath -ldflags "-s -w -X main.version=0.1.0+$commit" -o $out ./cmd/tenantscaler
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    Write-Output "built $out"
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
