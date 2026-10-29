<#
.SYNOPSIS
    Build do Kouki 1.0.2: gera o executavel e compila o instalador Inno Setup.

.DESCRIPTION
    Pipeline completo:

      1. localizar um toolchain Go consistente
      2. preparar o payload do MPV a partir da pasta do usuario (opcional)
      3. regenerar kouki.ico a partir do PNG de origem
      4. regenerar kouki_windows.syso (icone + versao + manifest) via goversioninfo
      5. resolver modulos
      6. go build -> Kouki.exe
      7. compilar o instalador com ISCC (installer\kouki.iss)
      8. montar dist\

    Todo passo e idempotente.

    NOTA: este script nao executa testes automatizados. O projeto nao os utiliza.

.PARAMETER MpvSource
    Pasta com o mpv.exe / portable_config do usuario. Se omitida (ou inexistente)
    o payload ja preparado em assets\mpv e reutilizado.

.PARAMETER IconSource
    PNG de origem do icone. Padrao: assets\icon\icon-512.png.

.PARAMETER SkipInstaller
    Compila apenas o executavel, sem chamar o ISCC.

.PARAMETER SkipStaging
    Nao popula dist\.

.EXAMPLE
    .\tools\build.ps1
    .\tools\build.ps1 -MpvSource "C:\Users\Flugel\Documents\mpv"
#>
[CmdletBinding()]
param(
    [string]$MpvSource,
    [string]$IconSource,
    [switch]$SkipInstaller,
    [switch]$SkipStaging
)

$ErrorActionPreference = 'Stop'
$root  = Split-Path -Parent $PSScriptRoot
$stamp = (Get-Date).ToString('HH:mm:ss')

function Step($msg) { Write-Host "[$stamp] ==> $msg" -ForegroundColor Cyan }
function Ok($msg)   { Write-Host "[$stamp]     $msg" -ForegroundColor DarkGray }
function Good($msg) { Write-Host "[$stamp]     OK  $msg" -ForegroundColor Green }
function Bad($msg)  { Write-Host "[$stamp]     !!  $msg" -ForegroundColor Red }

Set-Location $root

# --- 1. Go toolchain -------------------------------------------------------
Step 'Localizando o Go'

# Dois Go convivem nesta maquina e um GOROOT leftover de outra instalacao produz o
# confuso   compile: version "go1.27.2" does not match go tool version "go1.27.0"
# Por isso GOROOT e fixado no go.exe escolhido em vez de confiar no ambiente.
$candidates = @()
if ($env:GoExe -and (Test-Path $env:GoExe)) { $candidates += $env:GoExe }
foreach ($c in (where.exe go 2>$null)) { $candidates += $c }
foreach ($cand in @(
    "$env:ProgramFiles\Go\bin\go.exe",
    "$env:LOCALAPPDATA\Programs\go\bin\go.exe",
    "$env:USERPROFILE\go\bin\go.exe"
)) { if (Test-Path $cand) { $candidates += $cand } }

$best = $null
foreach ($c in ($candidates | Select-Object -Unique)) {
    if (-not (Test-Path $c)) { continue }
    $candidateRoot = Split-Path (Split-Path -Parent $c) -Parent
    if (-not (Test-Path (Join-Path $candidateRoot 'pkg\tool'))) { continue }
    $prev = $env:GOROOT
    $env:GOROOT = $candidateRoot
    $v = (& $c version 2>$null) -replace '^go version go',''
    $env:GOROOT = $prev
    if ($v -notmatch '(\d+\.\d+(?:\.\d+)?)') { continue }
    $parsed = [version]$Matches[1]
    if (-not $best -or $parsed -gt $best.ver) { $best = @{ exe = $c; ver = $parsed; raw = $v; root = $candidateRoot } }
}
if (-not $best) { Bad 'Nenhum Go utilizavel encontrado. Instale o Go 1.26+ e rode de novo.'; exit 1 }

$goExe    = $best.exe
$env:PATH  = (Split-Path -Parent $goExe) + ';' + $env:PATH
$env:GOROOT = $best.root
Good ("{0}  ({1})" -f $best.raw, $goExe)

# --- 2. MPV payload --------------------------------------------------------
if (-not $MpvSource) { $MpvSource = "$env:USERPROFILE\Documents\mpv" }
if (Test-Path (Join-Path $MpvSource 'mpv.exe')) {
    Step 'Preparando o payload do MPV'
    & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'tools\prepare-mpv.ps1') -Source $MpvSource | Out-Null
    if ($LASTEXITCODE -ne 0) { Bad 'prepare-mpv falhou'; exit 1 }
    $n = (Get-ChildItem (Join-Path $root 'assets\mpv') -Recurse -File | Measure-Object -Property Length -Sum)
    Good ("assets/mpv  {0} arquivos, {1:N1} MB" -f $n.Count, ($n.Sum / 1MB))
} else {
    Ok "MPV de origem nao encontrado em '$MpvSource' - reaproveitando o payload ja preparado"
    if (-not (Test-Path (Join-Path $root 'assets\mpv\mpv.exe'))) {
        Bad 'Nenhum payload de MPV preparado. O build distribuiria um pacote quebrado.'
        exit 1
    }
}

# --- 3. icone --------------------------------------------------------------
if (-not $IconSource) { $IconSource = Join-Path $root 'assets\icon\icon-512.png' }
if (Test-Path $IconSource) {
    Step 'Gerando o kouki.ico multi-resolucao'
    & $goExe run ./tools/mkicon -in $IconSource -out (Join-Path $root 'kouki.ico') | Out-Null
    if ($LASTEXITCODE -ne 0) { Bad 'mkicon falhou'; exit 1 }
    Good ("kouki.ico  {0:N0} bytes" -f (Get-Item (Join-Path $root 'kouki.ico')).Length)
} else {
    Ok "PNG de origem nao encontrado em '$IconSource' - reaproveitando o kouki.ico existente"
}

# --- 4. recursos (.syso) ---------------------------------------------------
Step 'Gerando kouki_windows.syso (icone + versao + manifest)'
$gvi = $null
foreach ($cand in @("$env:USERPROFILE\go\bin\goversioninfo.exe", "$env:LOCALAPPDATA\Programs\Kouki\tools\goversioninfo.exe")) {
    if (Test-Path $cand) { $gvi = $cand; break }
}
if (-not $gvi) {
    Step 'Instalando goversioninfo (uma vez)'
    & $goExe install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest 2>&1 | Out-Null
    $gvi = "$env:USERPROFILE\go\bin\goversioninfo.exe"
}
if (Test-Path $gvi) {
    & $gvi -64 -o kouki_windows.syso -icon kouki.ico -application-icon kouki.ico `
           -manifest kouki.exe.manifest -propagate-ver-strings versioninfo.json
    if ($LASTEXITCODE -ne 0) { Bad 'goversioninfo falhou'; exit 1 }
    Good ("kouki_windows.syso  {0:N0} bytes" -f (Get-Item (Join-Path $root 'kouki_windows.syso')).Length)
} else {
    Bad 'goversioninfo indisponivel - o exe ficaria sem icone nem metadados de versao.'
    exit 1
}

# --- 5. modulos ------------------------------------------------------------
Step 'Resolvendo modulos'
& $goExe mod tidy 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) { Bad 'go mod tidy falhou'; exit 1 }
Good 'go.mod / go.sum atualizados'

# --- 6. build --------------------------------------------------------------
Step 'Compilando Kouki.exe'
$sw = [Diagnostics.Stopwatch]::StartNew()
& $goExe build -trimpath -ldflags='-H=windowsgui -s -w' -o Kouki.exe
$sw.Stop()
if ($LASTEXITCODE -ne 0) { Bad 'go build falhou'; exit 1 }
Good ("Kouki.exe  {0:N1} MB em {1:N1}s" -f ((Get-Item .\Kouki.exe).Length/1MB), $sw.Elapsed.TotalSeconds)

# --- 7. instalador ---------------------------------------------------------
if ($SkipInstaller) {
    Ok 'Instalador ignorado (-SkipInstaller)'
} else {
    Step 'Compilando o instalador (Inno Setup)'
    $iscc = $null
    $isRoot = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe"
    if (Test-Path $isRoot) { $iscc = $isRoot }
    if (-not $iscc) {
        foreach ($cand in @("$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe")) {
            if (Test-Path $cand) { $iscc = $cand; break }
        }
    }
    if (-not $iscc) {
        Bad 'ISCC.exe nao encontrado. Instale o Inno Setup 6 (https://jrsoftware.org/isdl.php) ou use -SkipInstaller.'
        exit 1
    }
    Ok "usando $iscc"
    & $iscc (Join-Path $root 'installer\kouki.iss')
    if ($LASTEXITCODE -ne 0) { Bad 'ISCC falhou'; exit 1 }
    Good 'dist\Kouki-1.0.2-Setup.exe'
}

# --- 8. stage --------------------------------------------------------------
if ($SkipStaging) { Step 'Stage ignorado'; exit 0 }

Step 'Montando dist/'
$dist = Join-Path $root 'dist'
New-Item -ItemType Directory -Force -Path $dist | Out-Null

if (Test-Path (Join-Path $dist 'Kouki-1.0.2-Setup.exe')) {
    Good ("Kouki-1.0.2-Setup.exe  {0:N1} MB" -f ((Get-Item (Join-Path $dist 'Kouki-1.0.2-Setup.exe')).Length/1MB))
}
Copy-Item .\THIRD-PARTY-NOTICES.md $dist -Force
Copy-Item .\kouki.ico $dist -Force

@"
Kouki 1.0.2
===========

Instalar
--------
Execute Kouki-1.0.2-Setup.exe. O instalador faz a instalacao apenas para o seu
usuario e NAO pede privilegios de administrador:

    programa : %LOCALAPPDATA%\Programs\Kouki
    configs  : %APPDATA%\Kouki
    MPV      : %LOCALAPPDATA%\Programs\Kouki\mpv

Desmarque o componente MPV se quiser instalar so o programa.

Depois de instalar, use o atalho "Kouki" no Menu Iniciar. A janela principal abre
com todas as opcoes; fechar ou minimizar leva para a bandeja do sistema, e o item
"Abrir o Kouki" traz a janela de volta. Para sair de verdade: bandeja -> Sair
(ou o botao Sair na janela).

O MPV ja vem configurado com o perfil high-quality, uosc, shaders e o canal IPC
ligado para o Kouki ler. Nada precisa ser configurado manualmente.

Reinstalar / Desinstalar
------------------------
Configuracoes -> Aplicativos -> Kouki. De la voce pode Repair (reparar),
Modify (instalar/desinstalar o MPV) ou Uninstall.

Suas configuracoes e tokens do AniList/MAL sao mantidos na desinstalacao.
"@ | Set-Content (Join-Path $dist 'COMO-INSTALAR.txt') -Encoding UTF8
Good 'dist\COMO-INSTALAR.txt'

Step 'Concluido'
Get-ChildItem $dist | ForEach-Object {
    "     {0,-30} {1,10:N1} MB" -f $_.Name, ($_.Length / 1MB)
}
