# Let teammates' laptops reach the live-demo gateway port on this PC.
# The rule is inbound TCP on one port, Private network profile only (a phone
# hotspot), and is removed with -Remove after the demo. Run it only with the
# laptop owner's approval, from an elevated (Administrator) PowerShell:
#   ./scripts/demo/firewall.ps1 -Port 8081           # add the rule
#   ./scripts/demo/firewall.ps1 -Port 8081 -Remove   # remove it
param([int]$Port = 8081, [switch]$Remove)
$ErrorActionPreference = 'Stop'
$name = "TenantScaler live demo (TCP $Port)"

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { throw "Run this from an elevated (Administrator) PowerShell." }

$existing = Get-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue
if ($Remove) {
    if ($existing) { $existing | Remove-NetFirewallRule; Write-Output "Removed firewall rule: $name" }
    else { Write-Output "No firewall rule named '$name'." }
    exit 0
}
if ($existing) { Write-Output "Firewall rule already present: $name"; exit 0 }
New-NetFirewallRule -DisplayName $name -Direction Inbound -Protocol TCP -LocalPort $Port -Action Allow -Profile Private | Out-Null
Write-Output "Added inbound rule '$name' (Private profile only). Remove it after the demo: ./scripts/demo/firewall.ps1 -Port $Port -Remove"
