# Narrate, in plain words, what the live demo's backend is doing, for a
# terminal next to the dashboard (DEMO.md). It reads the dashboard's own API
# (no keys) and prints only what changed:
#   CONTROLLER  each tenant's grant and the controller's own reason
#   POOL        the pool size the controller applies
#   REPLICAS    llama.cpp replicas ready, starting and the target
#   TRAFFIC     per tenant every 10 s: requests/s, 429s, p99 time to first token
#   CPU         llama-server processes and the laptop's CPU, every 10 s
# Run it in its own PowerShell window while `demo -live` runs; Ctrl+C stops:
#   ./scripts/demo/explain.ps1
param([string]$Dashboard = 'http://127.0.0.1:8080', [int]$Every = 2)
$ErrorActionPreference = 'Stop'
$colors = @{ CONTROLLER = 'Cyan'; POOL = 'Cyan'; REPLICAS = 'Yellow'; TRAFFIC = 'Green'; CPU = 'Magenta'; TENANTS = 'White'; ERROR = 'Red' }

function Say([string]$Tag, [string]$Text) {
    Write-Host ((Get-Date -Format 'HH:mm:ss') + '  ') -NoNewline
    Write-Host ('{0,-10} ' -f $Tag) -ForegroundColor $colors[$Tag] -NoNewline
    Write-Host $Text
}

$cfg = Invoke-RestMethod -TimeoutSec 5 "$Dashboard/api/config"
Say 'TENANTS' ('backend: ' + $cfg.backend.label + ', budget ' + $cfg.capacityReplicas + ' replicas')
foreach ($t in $cfg.scenario.tenants) {
    $role = if ($t.name -eq $cfg.scenario.noisy) { 'noisy neighbour' } else { 'normal team' }
    Say 'TENANTS' ('{0}: {1} traffic, guaranteed {2} replica(s), max {3}, SLO first token <= {4} s  -> {5}' -f
        $t.name, $t.kind, $t.guaranteedReplicas, $t.maxReplicas, $t.ttftSeconds, $role)
}

$lastReason = @{}
$lastPool = ''
$lastReplicas = ''
$tick = 0
while ($true) {
    try {
        $state = Invoke-RestMethod -TimeoutSec 3 "$Dashboard/api/state"
        $stack = $state.stacks | Where-Object { $_.key -eq 'tenantscaler' }
        $sample = $stack.samples | Select-Object -Last 1
        if ($sample) {
            $replicas = '{0}|{1}|{2}' -f $sample.ready, $sample.starting, $sample.desired
            if ($replicas -ne $lastReplicas) {
                $lastReplicas = $replicas
                Say 'REPLICAS' ('llama.cpp replicas: {0} ready, {1} starting (loading the model), target {2} of budget {3}' -f
                    $sample.ready, $sample.starting, $sample.desired, $cfg.capacityReplicas)
            }
        }
        if ($tick % 5 -eq 0) {
            # The newest decision per tenant (records are newest first).
            $decisions = Invoke-RestMethod -TimeoutSec 3 "$Dashboard/api/decisions?limit=12"
            $newest = @{}
            foreach ($r in $decisions.records) {
                if (-not $newest.ContainsKey($r.tenant)) { $newest[$r.tenant] = $r }
            }
            foreach ($name in ($newest.Keys | Sort-Object)) {
                $reason = $newest[$name].reason -replace 'Gateway policy v\d+:', 'Gateway:'
                if ($name -eq $cfg.poolRecordTenant) {
                    if ($reason -ne $lastPool) { $lastPool = $reason; Say 'POOL' $reason }
                } elseif ($lastReason[$name] -ne $reason) {
                    $lastReason[$name] = $reason
                    if ($newest[$name].final.shedding) { $reason += '  [SHEDDING: extra requests get 429]' }
                    Say 'CONTROLLER' $reason
                }
            }
            if ($sample -and $sample.tenants) {
                $parts = @()
                foreach ($p in $sample.tenants.PSObject.Properties) {
                    $v = $p.Value
                    if ($v.rps -gt 0 -or $v.completed -gt 0 -or $v.shed -gt 0) {
                        $ttft = '-'
                        if ($null -ne $v.p99TtftSeconds) { $ttft = '{0:N1}s' -f $v.p99TtftSeconds }
                        $parts += ('{0}: {1:N1} req/s, {2} x 429, p99 TTFT {3}' -f $p.Name, $v.rps, $v.shed, $ttft)
                    }
                }
                if ($parts.Count -gt 0) { Say 'TRAFFIC' ($parts -join ' | ') }
            }
            $n = (Get-Process llama-server -ErrorAction SilentlyContinue | Measure-Object).Count
            $counter = '\Processor(_Total)\% Processor Time'
            $cpu = [math]::Round((Get-Counter $counter -SampleInterval 1 -MaxSamples 1).CounterSamples[0].CookedValue)
            Say 'CPU' ('{0} llama-server processes doing real inference; laptop CPU {1}%' -f $n, $cpu)
        }
    } catch {
        Say 'ERROR' $_.Exception.Message
    }
    $tick++
    Start-Sleep -Seconds $Every
}
