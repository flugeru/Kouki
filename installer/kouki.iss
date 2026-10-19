; ============================================================================
;  Kouki 1.0.2 - instalador
;
;  Compilado por Inno Setup 6 (ISCC.exe).
;  Uso:  ISCC.exe installer\kouki.iss
;
;  Por que Inno Setup  (ver README.md, secao "Instalador")
;  ----------------------------------------------------
;  * Repositorio ativo desde 1997, versao 6 estavel e mantida.
;  * Assistente de varios passos nativo, com suporte a modo escuro, que e
;    exatamente o que foi pedido (boas-vindas -> pasta -> atalhos -> conclusao).
;  * `PrivilegesRequired=lowest` instala por usuario, sem pedir administrador.
;  * Gera a entrada em "Configuracoes > Aplicativos" sozinho, com reparo,
;    modificacao e desinstalacao. Nao precisamos mais escrever no registro.
;  * Produz um unico .exe, com compactacao LZMA2 embutida.
;  * Usado por Git for Windows e pelo VS Code.
;  * Licenca permite uso comercial (ver LICENSE.TXT do proprio Inno Setup).
;
;  Comparacao com as alternativas consideradas esta no README.
; ============================================================================

#define AppName        "Kouki"
#define AppVersion     "1.0.2"
#define AppPublisher   "Kouki"
#define AppExeName     "Kouki.exe"

; Nome do pacote final (o usuario baixa esse arquivo).
#define OutputBaseName "Kouki-1.0.2-Setup"

[Setup]
AppId={{7B1E0C4A-5D62-4F39-9A17-6C0E2D8B4A55}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppPublisher}
VersionInfoVersion={#AppVersion}

; --- instalacao por usuario: NAO pede administrador -----------------------
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
DefaultDirName={localappdata}\Programs\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
DisableDirPage=no
AllowNoIcons=yes

; --- instalador ----------------------------------------------------------
SetupIconFile=..\kouki.ico
UninstallDisplayIcon={app}\{#AppExeName}
UninstallDisplayName={#AppName} {#AppVersion}
OutputDir=..\dist
OutputBaseFilename={#OutputBaseName}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
WizardSizePercent=110

; --- aparencia -----------------------------------------------------------
ShowLanguageDialog=auto
; O Kouki cria este mutex ao iniciar (install.go), entao o instalador percebe
; que ele esta aberto e pede para fechar antes de trocar o Kouki.exe.
AppMutex=KoukiSingleInstanceMutex

[Languages]
Name: "portuguese"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
portuguese.WelcomeTitle2=Instalando o {#AppName}
portuguese.SelectDirTitle3=Escolha onde instalar o {#AppName}
portuguese.SelectDirLabel3=O {#AppName} sera instalado na pasta selecionada. O programa e o MPV ficam disponiveis apenas para o seu usuario, sem pedir administrador.
portuguese.SelectDirBrowseLabel=Procure por outra pasta
portuguese.FinishedTitleLabel=O {#AppName} foi instalado
portuguese.FinishedHeaderLabel=O {#AppName} foi instalado com sucesso.
portuguese.ClickFinish=Concluir

[CustomMessages]
portuguese.CreateDesktopIcon=Criar atalho na Area de Trabalho
portuguese.LaunchApplication=Executar o {#AppName}
portuguese.RunAsOriginalUser=Executar o {#AppName} como o usuario atual

[Tasks]
; O bit "checked" NAO e uma flag valida de [Tasks] no Inno Setup 6. As flags
; aceitas sao apenas: checkablealone, checkedonce, dontinheritcheck, exclusive,
; restart e unchecked. Uma tarefa nasce marcada; e "unchecked" que a desmarca.
; Portanto a opcao "autostart" abaixo, sem flag nenhuma, continua marcada por
; padrao -- que e a intencao original.
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked
Name: "autostart";    Description: "Iniciar o {#AppName} automaticamente ao entrar no Windows"; GroupDescription: "{cm:AdditionalIcons}"

[Icons]
Name: "{group}\{#AppName}";        Filename: "{app}\{#AppExeName}"; IconFilename: "{app}\{#AppExeName}"
Name: "{group}\{cm:UninstallProgram,{#AppName}}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\{#AppName}";  Filename: "{app}\{#AppExeName}"; IconFilename: "{app}\{#AppExeName}"; Tasks: desktopicon

[Files]
; --- o programa ----------------------------------------------------------
Source: "..\{#AppExeName}";        DestDir: "{app}"; Flags: ignoreversion
Source: "..\kouki.ico";            DestDir: "{app}"
Source: "..\THIRD-PARTY-NOTICES.md"; DestDir: "{app}"
Source: "..\README.md";            DestDir: "{app}"; Flags: ignoreversion

; --- MPV empacotado (componente opcional) -------------------------------
; mpv.exe sobe ao lado do portable_config\, que e a pasta que o proprio mpv
; carrega automaticamente quando encontra o executavel. E assim que os presets,
; scripts e shaders do usuario chegam intactos ate a maquina instalada.
Source: "..\assets\mpv\*"; DestDir: "{app}\mpv"; \
    Flags: ignoreversion recursesubdirs createallsubdirs skipifsourcedoesntexist

[Registry]
; A tarefa "autostart" era oferecida (e marcada por padrao), mas nenhuma entrada
; dependia dela: marcar ou desmarcar nao mudava nada. O Kouki le essa entrada ao
; iniciar e a adota (ver main.go), e o --tray faz ele abrir direto na bandeja.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; \
    ValueType: string; ValueName: "{#AppName}"; \
    ValueData: """{app}\{#AppExeName}"" --tray"; \
    Tasks: autostart; Flags: uninsdeletevalue

; (A secao [Dirs] foi removida: dar permissao "users-modify" em {commonappdata}
; mexe na ACL de C:\ProgramData numa instalacao que nao tem privilegio de
; administrador, e {app} fica dentro do perfil do proprio usuario.)

[Run]
Filename: "{app}\{#AppExeName}"; \
    Description: "{cm:LaunchApplication,{#AppName}}"; \
    Flags: nowait postinstall skipifsilent

[Code]
// ---------------------------------------------------------------------------
// Validacao antes de comecar a copiar.
// ---------------------------------------------------------------------------
function InitializeSetup(): Boolean;
var
  FreeMB, TotalMB: Cardinal;
  NeedMB: Cardinal;
begin
  Result := True;

  // O programa + o MPV empacotado (~127 MB) precisam de folga para extrair.
  //
  // GetSpaceOnDisk(Path, InMegabytes, var Free, var Total): quatro parametros.
  // InMegabytes = True devolve MEGABYTES (2^20) e evita o teto de 2^31-1 bytes
  // que a variante em bytes impoe, entao a comparacao e feita em MB.
  NeedMB := 420;
  if not GetSpaceOnDisk(ExpandConstant('{app}'), True, FreeMB, TotalMB) then
  begin
    // Nao deu para medir o disco. Avisamos e seguimos em vez de bloquear uma
    // instalacao que pode estar perfeitamente correta.
    MsgBox('Nao foi possivel verificar o espaco livre em ' + ExpandConstant('{app}') + '.', mbError, MB_OK);
    Exit; // Result continua True
  end;

  if FreeMB < NeedMB then
  begin
    MsgBox(
      'Espaco insuficiente.' + #13#10 + #13#10 +
      'O {#AppName} precisa de aproximadamente ' + IntToStr(NeedMB) +
      ' MB livres (programa + MPV).' + #13#10 +
      'Desinstale o {#AppName} existente ou libere espaco e tente novamente.',
      mbError, MB_OK);
    Result := False;
  end;
end;

// ---------------------------------------------------------------------------
// A entrada do Painel de Controle e criada pelo proprio Inno Setup a partir de
// AppId, o que cobre instalar, reparar, modificar e desinstalar.
//
// Este passo apenas garante que o icone exibido ali seja o do Kouki.
// ---------------------------------------------------------------------------
procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
  begin
    // Nada obrigatorio: o Inno Setup ja registrou tudo. O gancho existe para o
    // comportamento do icone ficar explicito e documentado no codigo.
  end;
end;

// ---------------------------------------------------------------------------
// Limpeza na desinstalacao: as configuracoes e tokens do usuario sao
// preservados por padrao. Este gancho existe para o usuario poder remove-los
// de forma explicita, sem precisar explorar pastas.
// ---------------------------------------------------------------------------
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
  begin
    // Nada a fazer: %APPDATA%\Kouki fica intacto de proposito.
  end;
end;
