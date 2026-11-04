[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
Push-Location (Split-Path -Parent $PSScriptRoot)
try {
    cargo build --locked --release
    if ($LASTEXITCODE -ne 0) { throw 'Rust build failed' }
    New-Item -ItemType Directory -Force dist | Out-Null
    Copy-Item target\release\kouki.exe dist\Kouki-Rust.exe -Force
    Copy-Item README-Rust.md,LICENSE dist -Force
    Write-Host 'Build: dist\Kouki-Rust.exe (MPV externo; sem payload Go)'
} finally {
    Pop-Location
}
