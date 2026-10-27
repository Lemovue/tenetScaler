# Send one tenant's live traffic to the gateway in the cluster
# (scripts/k3s/expose.sh forwards it to port 8081), with that tenant's key
# from the key file of scripts/k3s/keys.ps1. The key is passed as
# $TS_API_KEY, never printed.
#   ./scripts/k3s/loadgen.ps1 -Tenant a -LoadProfile steady
#   ./scripts/k3s/loadgen.ps1 -Tenant b -LoadProfile periodic
#   ./scripts/k3s/loadgen.ps1 -Tenant c -LoadProfile spike -Extra '-spike-now', '-spike-duration', '60s'
param(
    [Parameter(Mandatory = $true)][ValidateSet('a', 'b', 'c')][string]$Tenant,
    [Parameter(Mandatory = $true)][ValidateSet('steady', 'periodic', 'spike')][string]$LoadProfile,
    [string]$Gateway = 'http://127.0.0.1:8081',
    [string]$Path = (Join-Path $env:USERPROFILE '.tenantscaler\k8s.env'),
    [string[]]$Extra = @()
)
$ErrorActionPreference = 'Stop'
# The binary scripts/build-dist.ps1 writes; scripts/build.ps1's as a fallback.
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$exe = @('dist\tenantscaler-windows-amd64.exe', 'bin\tenantscaler.exe') |
    ForEach-Object { Join-Path $root $_ } | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $exe) { throw "no tenantscaler binary: run ./scripts/build-dist.ps1 first" }

$line = Get-Content $Path | Where-Object { $_ -like 'TS_TENANT_KEYS=*' }
$entry = ($line -replace '^TS_TENANT_KEYS=', '') -split ',' | Where-Object { $_ -like "${Tenant}:*" }
if (-not $entry) { throw "no key for tenant $Tenant in $Path" }
$previous = $env:TS_API_KEY
$env:TS_API_KEY = ($entry -split '=', 2)[1]
try {
    & $exe loadgen -gateway $Gateway -profile $LoadProfile @Extra
} finally {
    if ($null -eq $previous) { Remove-Item Env:TS_API_KEY -ErrorAction SilentlyContinue }
    else { $env:TS_API_KEY = $previous }
}
