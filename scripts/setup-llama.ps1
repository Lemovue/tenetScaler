# Download the REAL inference engine (llama.cpp llama-server, CUDA 12.4 build)
# and the model (Qwen2.5-0.5B-Instruct, Q4_K_M GGUF, Apache-2.0) into .cache/.
# Versions and SHA-256 digests are pinned; files that already verify are not
# downloaded again. Set TS_CACHE_DIR to reuse a cache from another checkout.
# The two paths are then written to .env (created from .env.example when
# missing; other lines are kept), unless -NoEnv is given.
param([switch]$NoEnv)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$cache = if ($env:TS_CACHE_DIR) { $env:TS_CACHE_DIR } else { Join-Path $root '.cache' }

$build = 'b11898'
$base = "https://github.com/ggml-org/llama.cpp/releases/download/$build"
$files = @(
    @{ Name = "llama-$build-bin-win-cuda-12.4-x64.zip"; Url = "$base/llama-$build-bin-win-cuda-12.4-x64.zip";
       Sha = '7ff07768983fb73af6a52a001ca48effc0e03e716b1d5dc6f0b07b6f4f406464'; Extract = $true },
    @{ Name = 'cudart-llama-bin-win-cuda-12.4-x64.zip'; Url = "$base/cudart-llama-bin-win-cuda-12.4-x64.zip";
       Sha = '8c79a9b226de4b3cacfd1f83d24f962d0773be79f1e7b75c6af4ded7e32ae1d6'; Extract = $true },
    @{ Name = 'qwen2.5-0.5b-instruct-q4_k_m.gguf';
       Url = 'https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/9217f5db79a29953eb74d5343926648285ec7e67/qwen2.5-0.5b-instruct-q4_k_m.gguf';
       Sha = '74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db'; Extract = $false }
)

$dl = Join-Path $cache 'downloads'
$bin = Join-Path $cache "llama-$build"
$models = Join-Path $cache 'models'
New-Item -ItemType Directory -Force $dl, $bin, $models | Out-Null

function Test-Sha($path, $sha) {
    (Test-Path $path) -and ((Get-FileHash -Algorithm SHA256 $path).Hash.ToLower() -eq $sha)
}

foreach ($f in $files) {
    $target = if ($f.Extract) { Join-Path $dl $f.Name } else { Join-Path $models $f.Name }
    if (Test-Sha $target $f.Sha) {
        Write-Output "ok (cached)  $($f.Name)"
    } else {
        Write-Output "downloading  $($f.Name)"
        curl.exe -L --fail --retry 3 --max-time 3600 -o "$target.part" $f.Url
        if ($LASTEXITCODE -ne 0) { throw "download failed: $($f.Url)" }
        Move-Item -Force "$target.part" $target
        if (-not (Test-Sha $target $f.Sha)) { Remove-Item $target; throw "SHA-256 mismatch for $($f.Name)" }
        Write-Output "verified     $($f.Name)"
    }
    if ($f.Extract) { Expand-Archive -Force -Path $target -DestinationPath $bin }
}

$server = Get-ChildItem -Recurse -Filter llama-server.exe $bin | Select-Object -First 1
if (-not $server) { throw "llama-server.exe not found after extraction" }
Write-Output ""
Write-Output "llama-server : $($server.FullName)"
Write-Output "model        : $(Join-Path $models 'qwen2.5-0.5b-instruct-q4_k_m.gguf')"
$values = [ordered]@{
    TS_LLAMA_SERVER = $server.FullName
    TS_LLAMA_MODEL  = (Join-Path $models 'qwen2.5-0.5b-instruct-q4_k_m.gguf')
}
if ($NoEnv) {
    Write-Output "Set (or put in .env):"
    foreach ($k in $values.Keys) { Write-Output "  $k=$($values[$k])" }
    exit 0
}
$envFile = Join-Path $root '.env'
if (-not (Test-Path $envFile)) { Copy-Item (Join-Path $root '.env.example') $envFile }
$lines = [System.Collections.Generic.List[string]]@(Get-Content $envFile)
foreach ($k in $values.Keys) {
    $i = $lines.FindIndex({ param($l) $l -match "^\s*$k\s*=" })
    if ($i -ge 0) { $lines[$i] = "$k=$($values[$k])" } else { $lines.Add("$k=$($values[$k])") }
}
[System.IO.File]::WriteAllLines($envFile, $lines, (New-Object System.Text.UTF8Encoding $false))
Write-Output "wrote TS_LLAMA_SERVER and TS_LLAMA_MODEL to $envFile"
