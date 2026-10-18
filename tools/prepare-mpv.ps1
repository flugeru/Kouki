<#
    prepare-mpv.ps1 — builds the curated MPV payload at assets/mpv/.

    This is part of the documented build so the bundled MPV is reproducible.
    It NEVER modifies the user's source MPV folder; it only reads from it.

    What is bundled  : mpv.exe, mpv.com, d3dcompiler_43.dll, fonts.conf,
                       portable_config/{fonts,scripts,script-opts,shaders}
    What is excluded  : the mpv.io auto-updater scaffolding (7z\, installer\,
                        *.bat, settings.xml, doc\) because it would overwrite our
                        bundled copy; portable_config\cache (~450 MB of generated
                        shader cache, regenerated on demand); watch_later (playback
                        history); uosc's darwin/linux binaries.

    Usage:
        .\tools\prepare-mpv.ps1 -Source "C:\Users\Flugel\Documents\mpv"
#>
[CmdletBinding()]
param(
    [string]$Source = "$env:USERPROFILE\Documents\mpv"
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dest = Join-Path $root 'assets\mpv'

if (-not (Test-Path (Join-Path $Source 'mpv.exe'))) {
    throw "mpv.exe not found in '$Source'. Pass -Source <path to mpv folder>."
}

Write-Host "Source : $Source"
Write-Host "Dest   : $dest"

# --- executables / runtime -------------------------------------------------
foreach ($f in @('mpv.exe', 'mpv.com', 'd3dcompiler_43.dll')) {
    $s = Join-Path $Source $f
    if (-not (Test-Path $s)) { Write-Warning "missing: $f"; continue }
    Copy-Item $s (Join-Path $dest $f) -Force
    Write-Host "  + $f"
}

# --- fontconfig -----------------------------------------------------------
Copy-Item (Join-Path $Source 'mpv\fonts.conf') (Join-Path $dest 'fonts.conf') -Force
Write-Host "  + fonts.conf"

# --- portable_config subtrees ---------------------------------------------
$pcSrc = Join-Path $Source 'portable_config'
$pcDst = Join-Path $dest 'portable_config'

foreach ($sub in @('fonts', 'script-opts', 'shaders')) {
    $s = Join-Path $pcSrc $sub
    if (Test-Path $s) {
        $d = Join-Path $pcDst $sub
        New-Item -ItemType Directory -Force -Path $d | Out-Null
        Copy-Item (Join-Path $s '*') $d -Recurse -Force
        Write-Host "  + portable_config/$sub"
    }
}

# scripts: copy, then prune the non-Windows uosc helper binaries
$s = Join-Path $pcSrc 'scripts'
if (Test-Path $s) {
    $d = Join-Path $pcDst 'scripts'
    New-Item -ItemType Directory -Force -Path $d | Out-Null
    Copy-Item (Join-Path $s '*') $d -Recurse -Force
    foreach ($junk in @('ziggy-darwin', 'ziggy-linux')) {
        $p = Join-Path $d "uosc\bin\$junk"
        if (Test-Path $p) { Remove-Item $p -Force; Write-Host "  - uosc/bin/$junk (non-Windows)" }
    }
    Write-Host "  + portable_config/scripts"
}

Write-Host ""
Write-Host "Resulting payload:"
Get-ChildItem $dest -Recurse -File |
    Measure-Object -Property Length -Sum |
    ForEach-Object { "  {0} files, {1:N1} MB" -f $_.Count, ($_.Sum / 1MB) }
