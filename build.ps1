# build.ps1 — build llama-swappy (see README.md).
# Default output: llama-swappy.exe in the repository root.
# Optional output path: .\build.ps1 -OutPath D:\some\dir\llama-swappy.exe
param(
    [string]$OutPath = 'llama-swappy.exe'
)

$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

go build -o $OutPath .
if ($LASTEXITCODE -ne 0) {
    Write-Error "go build failed with exit code $LASTEXITCODE"
    exit $LASTEXITCODE
}
Write-Output "Built $OutPath"
