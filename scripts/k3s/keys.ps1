# Create the key file for the cluster's Secret tenantscaler-keys, OUTSIDE the
# repository: one random key per tenant (a, b, c: lower case, the
# TenantScaler names in deploy/k8s/40-tenants.yaml) and an admin key. Keys are
# never printed. -Copy puts one tenant's key on the clipboard, to send to that
# teammate only. Windows clipboard history (Win+V) keeps it, so make new keys
# with -Force after the demo.
#   ./scripts/k3s/keys.ps1            # create the file (refuses to overwrite)
#   ./scripts/k3s/keys.ps1 -Force     # new keys (then run scripts/k3s/up.sh again)
#   ./scripts/k3s/keys.ps1 -Copy c    # tenant c's key to the clipboard
param(
    [string]$Path = (Join-Path $env:USERPROFILE '.tenantscaler\k8s.env'),
    [switch]$Force,
    [ValidateSet('a', 'b', 'c')][string]$Copy
)
$ErrorActionPreference = 'Stop'
# One absolute path for every call below: .NET's [IO.File] does not follow
# Set-Location.
$Path = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)

function New-Key {
    $bytes = New-Object byte[] 16
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    $rng.GetBytes($bytes)
    $rng.Dispose()
    -join ($bytes | ForEach-Object { $_.ToString('x2') })
}

if ($Copy) {
    $line = Get-Content $Path | Where-Object { $_ -like 'TS_TENANT_KEYS=*' }
    $entry = ($line -replace '^TS_TENANT_KEYS=', '') -split ',' | Where-Object { $_ -like "${Copy}:*" }
    if (-not $entry) { throw "no key for tenant $Copy in $Path" }
    ($entry -split '=', 2)[1] | Set-Clipboard
    Write-Output "Tenant $Copy's key is on the clipboard (not printed). Send it to that teammate only."
    exit 0
}
if ((Test-Path $Path) -and -not $Force) { throw "$Path exists; use -Force to make new keys" }
New-Item -ItemType Directory -Force (Split-Path $Path) | Out-Null
$text = "TS_TENANT_KEYS=a:standard=$(New-Key),b:standard=$(New-Key),c:sheddable=$(New-Key)`nTS_ADMIN_KEY=$(New-Key)`n"
# LF and no BOM: kubectl --from-env-file would keep a CR in the values.
[IO.File]::WriteAllText($Path, $text, (New-Object Text.UTF8Encoding $false))
Write-Output "Wrote $Path (keys not printed)."
