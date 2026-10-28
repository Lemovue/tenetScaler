# Bring the side-by-side Kubernetes demo up (again), from Windows PowerShell
# in the repository, after the cluster is deployed (deploy/k3s/README.md):
# three windows that must stay open, then the dashboard in the browser.
#   1. WSL: scripts/k3s/expose.sh side-by-side (gateways and controller on
#      127.0.0.1:18081/18082/18090, for the dashboard)
#   2. WSL: scripts/k3s/mirror.sh (teammates' port 8081, no port-forward)
#   3. Windows: the dashboard on http://127.0.0.1:8080 watching the cluster
# Closing every WSL window can let WSL stop the VM, and k3s with it; run this
# script again and the cluster comes back by itself.
#   ./scripts/k3s/demo-up.ps1                    # replicas are llama.cpp pods
#   ./scripts/k3s/demo-up.ps1 -Replicas simulated
param(
    [ValidateSet('llama', 'simulated')][string]$Replicas = 'llama',
    [string]$Distro = 'Ubuntu-24.04',
    [string]$Keys = (Join-Path $env:USERPROFILE '.tenantscaler\k8s.env')
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
foreach ($port in 8080, 8081) {
    if (Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue) {
        throw "port $port is in use (a Windows demo -live?); stop it first"
    }
}
# After a network change the node may still hold its old address: heal.sh
# restarts k3s (and KEDA) when it does. This also starts the VM if WSL stopped it.
& wsl -d $Distro -u root --cd $root -- bash scripts/k3s/heal.sh
Start-Process wsl -ArgumentList '-d', $Distro, '--cd', $root, '--', 'bash', 'scripts/k3s/expose.sh', 'side-by-side'
Start-Process wsl -ArgumentList '-d', $Distro, '--cd', $root, '--', 'bash', 'scripts/k3s/mirror.sh', 'dist/tenantscaler-linux-amd64'
$up = $false
for ($i = 0; $i -lt 60 -and -not $up; $i++) {
    Start-Sleep -Seconds 2
    try {
        foreach ($u in 'http://127.0.0.1:18081/healthz', 'http://127.0.0.1:18082/healthz', 'http://127.0.0.1:18090/healthz', 'http://127.0.0.1:8081/healthz') {
            Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 $u | Out-Null
        }
        $up = $true
    } catch {}
}
if (-not $up) { throw "the cluster's gateways did not answer within 2 minutes: check the two WSL windows" }
# The dashboard's axes: the cluster's budget (2 per pool with llama pods).
$cfg = if ($Replicas -eq 'llama') { 'config/k3s-llama.yaml' } else { 'config/live-demo.yaml' }
$dash = "bin\tenantscaler.exe demo -config $cfg -observe-gateway http://127.0.0.1:18081 " +
    "-observe-controller http://127.0.0.1:18090 -observe-baseline-gateway http://127.0.0.1:18082 " +
    "-observe-replicas $Replicas -observe-keys `"$Keys`""
Start-Process powershell -WorkingDirectory $root -ArgumentList '-NoExit', '-Command', $dash
# The narrated "backend" view for the judges (scripts/k3s/explain.py).
$wslKeys = '/mnt/' + $Keys.Substring(0, 1).ToLower() + $Keys.Substring(2).Replace('\', '/')
Start-Process wsl -ArgumentList '-d', $Distro, '--cd', $root, '--', 'python3', 'scripts/k3s/explain.py', '--keys', $wslKeys
Start-Sleep -Seconds 3
Start-Process 'http://127.0.0.1:8080'
Write-Output "Up: teammates use http://<laptop IP>:8081 with their cluster keys; dashboard on http://127.0.0.1:8080; the explain window narrates the backend. Keep the windows open."
