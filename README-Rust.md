# Kouki Rust — 2.0.0-alpha.1

Branch de reescrita: `rewrite/rust-multiplatform`. Windows e Linux primeiro;
Android (`is.xyz.mpv`) será integrado depois de validar IPC entre aplicativos.
Não há suporte macOS nem APK nesta etapa.

## Executar

Instale Rust estável e MPV. Windows requer ferramentas de compilação MSVC.
Linux Debian/Ubuntu:

```sh
sudo apt install build-essential pkg-config libxcb-render0-dev libxcb-shape0-dev libxcb-xfixes0-dev libxkbcommon-dev libssl-dev libwayland-dev libegl1-mesa-dev mpv
cargo run --locked
```

Windows:

```powershell
cargo run --locked
```

Em Configurações, informe caminho do MPV (`mpv.exe` no Windows se estiver no
PATH). Clique em Salvar. A opção Iniciar MPV passa `--input-ipc-server` ao
player, sem modificar seu `mpv.conf`. Para acompanhar um MPV aberto fora do
Kouki, copie a linha IPC mostrada na interface para o `mpv.conf` e reinicie MPV.

MPV é separado; seus scripts, codecs e configurações continuam sob controle do
usuário. Esta versão Rust não redistribui MPV nem depende de Go.

## Contas

- AniList usa autorização pelo navegador com `state` aleatório. Client ID deve
  registrar `http://127.0.0.1:45124/callback`.
- MAL exige client ID de aplicativo público/nativo com PKCE e callback
  `http://127.0.0.1:45125/callback`. Não embuta client secret num app desktop.
- Progresso é enviado ao atingir 80%, com sucesso separado por serviço e nova
  tentativa após falha. Correção manual fica associada ao título e temporada.
- Identificação automática é heurística. Confira resultado antes de habilitar
  sincronização em contas importantes.

## Dados e segurança

Dados ficam em `%APPDATA%\Kouki` ou `$XDG_CONFIG_HOME/Kouki` (padrão
`~/.config/Kouki`). `rust-settings.json` e `rust-auth.json` são independentes dos
arquivos Go. Se `rust-auth.json` não existir, `auth.json` legado é lido sem ser
alterado. Tokens ficam em arquivo privado, mas ainda não em cofre do sistema;
não compartilhe esse arquivo. Windows usa ACLs herdadas do perfil do usuário.

Simulwatching Rust usa protocolo novo, não compatível com versão Go. Até 8
conexões, mensagens limitadas, convite aleatório e cancelamento por sessão.
TCP não tem TLS: use somente localhost, LAN confiável ou VPN. Não abra porta
pública. UPnP automático foi removido deliberadamente. Sincronizar progresso
do anfitrião exige autorização explícita na interface.

Discord Rich Presence exige cliente desktop em execução. Android não possui
equivalente garantido ao IPC do Discord desktop.

## Verificar e compilar

```sh
cargo fmt --check
cargo clippy --locked --all-targets -- -D warnings
cargo test --locked
cargo build --locked --release
```

Saída: `target/release/kouki.exe` ou `target/release/kouki`.
`tools/build-rust.ps1` monta executável portátil Windows em `dist`.
`tools/build-rust-appimage.sh` gera AppImage com `appimagetool` já instalado;
usa bibliotecas gráficas do sistema e ainda requer validação na distribuição destino.
CI executa testes e publica binários Windows/Linux. Validação gráfica,
autorização OAuth e reprodução real ainda precisam de smoke tests manuais.

## Migração em andamento

Já portado: janela desktop egui (sem navegador/WebView), MPV externo,
identificação e correção AniList/MAL, OAuth, progresso, Discord,
consulta Suwayomi, aviso de filler por título exato, sessão Simulwatching e persistência atômica.

Ainda pendente de paridade: bandeja e autostart, aliases/cache de filler,
reconhecimento avançado de releases com episódio zero, instalador Windows,
validação AppImage, armazenamento de tokens no cofre do sistema e Android.
Código Go é referência preservada, não dependência do binário Rust.
