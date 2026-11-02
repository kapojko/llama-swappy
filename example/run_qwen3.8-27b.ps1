# Windows equivalent of run_qwen3.8-27b.sh — llama-server launcher for
# llama-swappy. Config usage:
#
#   cmd: powershell -NoProfile -ExecutionPolicy Bypass -File C:\run\run_qwen3.8-27b.ps1 -Port ${PORT}
#
param(
    [int]$Port = 12380
)

# llama-swappy metadata hint: a no-op at runtime, but llama-swappy parses
# its arguments for /llama-swappy/info. Only needed when the real flags are
# hidden inside another tool's config (see README). The explicit flags in
# $serverArgs below take precedence for the parser (last occurrence wins),
# so keep this commented out unless you need it.
# llama-swappy-metadata -c 114688 -np 1
function llama-swappy-metadata {}

# Enforce offline mode so HF never attempts a re-download
$env:HF_HUB_OFFLINE = '1'

# Paths
$modelPath = "$env:USERPROFILE\models\ISTA-DASLab--Qwen3.8-27B-GSQ-RCO-GGUF\Qwen3.8-27B-GSQ-RCO-IQ3_XXS-mtp.gguf"
$serverBin = "$env:USERPROFILE\llama.cpp\build\bin\llama-server.exe"

# Check if model exists
if (-not (Test-Path $modelPath)) {
    Write-Error "Model not found at $modelPath"
    exit 1
}

# Server run arguments
$serverArgs = @(
    '-m', $modelPath
    '--no-mmproj'

    '--host', '0.0.0.0'
    '--port', $Port

    '-ngl', 'all'
    '-kvo'
    '--fit', 'off'
    '-fa', 'on'

    #'-c' '98304'
    '-c', '114688'
    #'-c' '122880'
    #'-c' '131072'
    #'-ctk' 'q8_0' '-ctv' 'q8_0'
    '-ctk', 'q5_1', '-ctv', 'q5_1'
    '-np', '1'
    '-b', '2048', '-ub', '512'

    '--spec-type', 'draft-mtp'
    '--spec-draft-n-max', '3'
    '--n-gpu-layers-draft', 'all'

    '--reasoning', 'on'
    '--reasoning-preserve'
    '--jinja'
    '--chat-template-kwargs', '{\"reasoning_effort\":\"medium\"}'

    '--temp', '1.0'
    '--top-p', '0.95'
    '--top-k', '20'
    '--min-p', '0.0'
    '--presence-penalty', '0.0'
    '--repeat-penalty', '1.0'

    '-t', "$env:NUMBER_OF_PROCESSORS"
    '--cache-ram', '16384'
)

& $serverBin @serverArgs
