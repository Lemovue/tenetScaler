# Download the real trace used by `tenantscaler forecast-trace` (PLAN.md
# decision Q18-A): the Azure LLM inference trace 2024, code, one week
# (github.com/Azure/AzurePublicDataset, release dataset-llm-2024; CC-BY;
# cite Stojkovic et al., "DynamoLLM", HPCA 2025). 692 MB into .cache/traces/;
# size and SHA-256 are pinned, an interrupted download resumes.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dir = Join-Path $root '.cache/traces'
New-Item -ItemType Directory -Force $dir | Out-Null
$name = 'AzureLLMInferenceTrace_code_1week.csv'
$url = "https://github.com/Azure/AzurePublicDataset/releases/download/dataset-llm-2024/$name"
$size = 691989454
$sha = '71de5c55cbc35f8f1ed0b6b7806b4cd1e9764b0058469725a6aac98023a1448f'
$target = Join-Path $dir $name

function Test-Trace($path) {
    (Test-Path $path) -and ((Get-Item $path).Length -eq $size) -and
        ((Get-FileHash -Algorithm SHA256 $path).Hash.ToLower() -eq $sha)
}

if (Test-Trace $target) { Write-Output "ok (cached)  $target"; exit 0 }
curl.exe -L --fail -C - --retry 10 --retry-all-errors --retry-delay 5 --max-time 7200 -o "$target.part" $url
if ($LASTEXITCODE -ne 0) { throw "download failed: $url" }
Move-Item -Force "$target.part" $target
if (-not (Test-Trace $target)) { throw "size or SHA-256 mismatch for $name" }
Write-Output "verified     $target"
