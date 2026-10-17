# Kouki

**Contas, reprodução e progresso** — mostra o que está tocando com o título, o
episódio, a capa e o progresso, e sincroniza com AniList e MyAnimeList. Tudo em
português, instalado só para o seu usuário, sem pedir administrador.

> Fork do [Koushin](https://github.com/hyuzipt/Koushin). As configurações antigas
> do Koushin são migradas automaticamente para `%APPDATA%\Kouki` na primeira
> execução. Os arquivos antigos não são apagados.

Repositório: <https://github.com/flugeru/Kouki>

---

## Sumário

- [Capturas de tela](#capturas-de-tela)
- [Instalação](#instalação)
- [Primeira execução](#primeira-execução)
- [Janela principal e bandeja](#janela-principal-e-bandeja)
- [A interface](#a-interface)
- [Instalador: por que Inno Setup](#instalador-por-que-inno-setup)
- [Integração com o MPV](#integração-com-o-mpv)
- [Dependências](#dependências)
- [Compilando](#compilando)
- [Estrutura do projeto](#estrutura-do-projeto)
- [Solução de problemas](#solução-de-problemas)
- [Licenças](#licenças)

---

## Capturas de tela

### Janela principal

![Janela principal do Kouki](assets/screenshots/interface.png)

### Rich presence

![Rich presence](assets/screenshots/rich-presence.png)

---

## Instalação

Execute **`Kouki-1.0.0-Setup.exe`**.

O instalador é *per-user*: grava em `%LOCALAPPDATA%\Programs\Kouki` e **não pede
privilégios de administrador**.

| Caminho | Conteúdo |
|---|---|
| `%LOCALAPPDATA%\Programs\Kouki` | programa, ícone, `THIRD-PARTY-NOTICES.md` |
| `%LOCALAPPDATA%\Programs\Kouki\mpv` | MPV empacotado (componente opcional) |
| `%APPDATA%\Kouki` | `auth.json` (configurações e tokens), `kouki.log` |

Na página de tarefas você pode escolher:

- **componente MPV** — desmarque para instalar só o programa (o Kouki então
  observa um MPV que você já tenha);
- **atalho na Área de Trabalho**;
- **iniciar com o Windows**.

Para desinstalar: *Configurações → Aplicativos → Kouki* (ou *Programas e
Recursos* no Painel de Controle). Ficam **Repair**, **Modify** e **Uninstall**.
Suas configurações e tokens são preservados.

---

## Primeira execução

O MPV já vem configurado — perfil `high-quality`, `uosc`, shaders, codecs e
`hwdec` — com o canal IPC ligado para o Kouki ler. Nada precisa ser ajustado.

Na janela principal, **Reprodução → Iniciar MPV** sobe o player empacotado.
Isso é o que garante que o `portable_config/` seja carregado: o mpv só procura
essa pasta ao lado do próprio executável, então ele precisa ser o binário que o
Kouki lançou.

---

## Janela principal e bandeja

A janela principal expõe **tudo** que antes só existia na bandeja:

| Seção | Ações |
|---|---|
| Reprodução | Iniciar/Encerrar MPV, Pausar/Retomar, procurar MPV, arquivo e posição atuais |
| AniList | Entrar/Sair, selecionar anime correto, acompanhar progresso, aviso de filler |
| MyAnimeList | Entrar/Sair, selecionar anime correto, acompanhar progresso |
| Simulwatching | Hospedar/Entrar/Parar, sincronizar AniList |
| Geral | Iniciar com o Windows, abrir pastas, reinstalar/desinstalar, Sobre, Sair |

Comportamento de fechar e minimizar:

- **Fechar** e **minimizar** ocultam a janela; o Kouki **continua na bandeja**.
- **bandeja → "Abrir o Kouki"** traz a janela de volta.
- Só **"Sair"** (na bandeja ou na janela) encerra de fato — e nesse momento o MPV
  que o Kouki iniciou é finalizado graciosamente.

A bandeja continua sendo o caminho rápido: as mesmas ações, em um clique.

---

## A interface

A janela é **uma superfície de desenho só** (`walk.CustomWidget` em `ui.go`), não
uma árvore de controles do Win32. Isso não é estilo: é uma restrição do toolkit.

- O `walk` **não expõe cor de texto** — `WidgetBase` não tem `SetForegroundColor`.
  Um `Label` sobre um painel escuro continua preto, e `Button`/`CheckBox`
  continuam com as cores do sistema (botões brancos).
- O `CustomWidget` **não tem tamanho próprio**: o item de layout dele é um
  `greedyLayoutItem` com `MinSize` fixo de 50x50, que ignora `SetMinMaxSize`.

Desenhando tudo num único canvas, cores, raios de canto, alturas e espaçamentos
viram aritmética de pixels, e a janela continua sendo uma janela nativa de
verdade — com message loop, bandeja, diálogos e tooltip do sistema.

O que isso **não** entrega:

- seleção de texto e cursor nas linhas somente-leitura;
- metadados de acessibilidade (UI Automation) dos controles desenhados;
- seguir os temas de alto contraste, cuja paleta substituiria a de `ui.go`;
- a janela não se ajusta ao conteúdo: ela é redimensionável e o conteúdo rola
  com a roda do mouse, o que mantém o layout idêntico em qualquer tamanho.

Paleta, tipografia (Segoe UI) e ícones (Segoe MDL2 Assets) são as do `ui.go`.

### Repaint

Mover o cursor pela janela é o caso mais quente que existe, e três decisões o
tornam barato:

1. **`PaintNoErase`.** O padrão do walk, `PaintNormal`, deixa o `WM_ERASEBKGND`
   apagar a superfície **antes** do `WM_PAINT` — duas mensagens distintas, com
   uma janela em branco entre elas, que o compositor do desktop pode amostrar.
2. **Dano parcial.** `walk.Invalidate()` só faz `InvalidateRect(hWnd, nil, true)`
   — o cliente inteiro. O `ui.go` chama `InvalidateRect` com o retângulo de cada
   controle afetado, então cruzar dois botões suja ~40x32 em vez de 1,4 milhão
   de pixels. O `paint()` usa a região danificada para descartar o resto do
   trabalho.
3. **Nada no caminho quente.** `WidgetBase.ToolTipText()` não é um getter: ele
   aloca 2 KB e envia `TTM_GETTOOLINFO`, uma `SendMessage` **síncrona**. O
   tooltip só é reescrito quando o controle sob o cursor muda, nunca a cada
   evento de mouse.

Some-se a isso a assinatura de estado em `InvalidateIfChanged`: o polling de
750 ms do player só redesenha quando algo exibido realmente mudou.

---

## Instalador: por que Inno Setup

**Escolhido: [Inno Setup 6](https://jrsoftware.org/isinfo.php)** — `installer/kouki.iss`.

Alternativas avaliadas:

| Ferramenta | Avaliação |
|---|---|
| **Inno Setup 6** | ✅ **Escolhido.** Assistente nativo de vários passos, modo escuro, per-user sem admin, entrada automática em "Aplicativos", LZMA2, um `.exe` só, ativo desde 1997. |
| **NSIS** | Script de compilação próprio e instalador em assembly de baixo nível. O assistente precisa ser montado à mão para ficar bonito — mais trabalho para um resultado pior. Licença MIT. |
| **WiX Toolset** | Gera `.msi` (Windows Installer), voltado a *machine-wide*. Instalação per-user via MSI é dolorosa e frequentemente exige elevação. Compiler XML, muito mais verboso. Licença MS-RL. |

Justificativa do Inno Setup:

1. **Wizard nativo de vários passos, com suporte a modo escuro.** Bate exatamente
   com o pedido: boas-vindas → seleção de pasta → tarefas/atalhos → conclusão, em
   português (`compiler:Default.isl` + `[CustomMessages]`).
2. **`PrivilegesRequired=lowest`** — instalação por usuário de verdade, sem UAC.
3. **Grava sozinho a entrada em "Aplicativos"**, com Repair/Modify/Uninstall. O
   código antigo escrevia `HKCU\...\Uninstall` à mão; isso não existe mais.
4. **Um `.exe` só**, com LZMA2 embutido.
5. **Maturity**: projeto ativo desde 1997, versão 6 estável, full source no
   GitHub, usado pelo Git for Windows e pelo VS Code.
6. **Licença** permite uso comercial (ver `LICENSE.TXT` do próprio Inno Setup).

Instalar o Inno Setup: <https://jrsoftware.org/isdl.php>

---

## Integração com o MPV

### Transporte

O MPV roda como **processo separado** e o Kouki fala com ele pelo **JSON IPC do
próprio mpv**, exposto como named pipe (`\\.\pipe\mpv-pipe`, configurado em
`assets/mpv/portable_config/mpv.conf`).

### Por que não usar bindings de libmpv

Bindings foram pesquisados e avaliados — o melhor candidato é
[`github.com/gen2brain/go-mpv`](https://github.com/gen2brain/go-mpv) (MIT,
suporta `purego` sem cgo). Foi **descartado** por três motivos concretos:

1. **Não existe `libmpv-2.dll` aqui.** O `mpv.exe` empacotado é o build do
   usuário (shinchiro) e **não** traz biblioteca dinâmica. A única fonte seria um
   `mpv-dev-*.7z` de terceiro, de um build *diferente* — o que contradiz
   exatamente o requisito de empacotar o MPV do usuário com os presets dele.
2. **Licença.** libmpv é LGPLv2.1+, e o `mpv.exe` empacotado é **GPLv2+**.
   Ligar libmpv obrigaria a aplicação inteira a GPLv2+, uma mudança de licença
   que o projeto não pode fazer em nome do usuário.
3. **Vídeo embutido.** Incorporar vídeo exige um HWND filho nativo, e uma
   superfície WebView2 (DirectComposition) não faz composição de janelas filhas
   estrangeiras sobre o conteúdo web.

O JSON IPC é a superfície de integração **oficial** para um player externo,
funciona com o MPV empacotado e com qualquer MPV que o usuário já tenha, e não
exige toolchain C.

### O que foi corrigido

| Antes | Agora |
|---|---|
| Respostas casadas por `request_id != 0`, então um **evento** assíncrono era aceito como resposta e dessincronizava o canal inteiro | Casamento estrito por `request_id`; eventos são enfileirados em `drainEvents()` |
| Sem serialização: o loop de 250 ms e a GUI podiam se intercalar no fio | `sync.Mutex` por requisição — o loop e a janela dividem uma conexão com segurança |
| `isPipeGone` comparava **texto** de erro em inglês (`"broken pipe"`, `"eof"`) — quebrava em silêncio com qualquer mudança de mensagem | Classificação por `errors.Is`/`errors.As` sobre `syscall.Errno` e `winio.ErrTimeout` |
| Kouki **não** iniciava o MPV; o usuário tinha que abrir à mão, e aí o `portable_config` nem sempre era usado | `mpvPlayer.Launch`/`Stop`: inicia com `--force-window`, `--hwdec=auto-safe` e `cmd.Dir` no diretório do MPV; `Stop` pede `quit` pelo IPC e só então força |
| Estado lido por 6 idas e voltas a cada 250 ms | Mantido, mas via `mpvConn.request` com deadlines por chamada e erros individuais tolerados (propriedades ausentes em stream ao vivo) |

Dependência trocada: **`github.com/natefinch/npipe` (2016, sem manutenção) →
`github.com/Microsoft/go-winio`**, mantida pela Microsoft, com `DialPipe` e
suporte completo a named pipes do Windows.

---

## Dependências

| Módulo | Licença | Por que |
|---|---|---|
| `github.com/lxn/walk` | MIT | Janela principal **nativa** do Windows, Go puro (sem cgo). Fornece a janela, o laço de mensagens e uma superfície de desenho (`CustomWidget`); a interface em si é desenhada à mão em `ui.go`. |
| `github.com/Microsoft/go-winio` | MIT | Named pipes do Windows, mantida pela Microsoft (substitui o `npipe` de 2016). |
| `github.com/getlantern/systray` | Apache-2.0 | Ícone e menu da bandeja. Mantido porque a lógica de menu já existe e é testada. |
| `github.com/5rahim/habari` | MIT | Parser de nomes de anime/filmes. |
| `github.com/huin/goupnp`, `github.com/jackpal/go-nat-pmp` | MIT | Descoberta de IP público e port-forward (Simulwatching). |
| `golang.org/x/sys` | BSD-3 | Chamadas de sistema do Windows. |

**Removidos:** `github.com/jchv/go-webview2` (instalador interno) e
`github.com/natefinch/npipe`.

---

## Compilando

Requisitos: **Go 1.26+** e **Inno Setup 6** (só para gerar o instalador).

```powershell
# build completo: exe + instalador
.\tools\build.ps1

# reaproveitando o MPV já preparado em assets\mpv
.\tools\build.ps1 -SkipInstaller

# só o instalador, reaproveitando o exe já compilado
iscc installer\kouki.iss
```

Saída em `dist\`:

- `Kouki-1.0.0-Setup.exe` — instalador
- `COMO-INSTALAR.txt`
- `kouki.ico`, `THIRD-PARTY-NOTICES.md`

> **Atenção:** dois Go convivem nesta máquina. O `build.ps1` escolhe o mais novo
> e **fixa `GOROOT`** de propósito; um `GOROOT` deixado de outra instalação gera
> `compile: version "go1.27.2" does not match go tool version "go1.27.0"`.

Para regenerar o payload do MPV a partir do seu MPV:

```powershell
.\tools\prepare-mpv.ps1 -Source "$env:USERPROFILE\Documents\mpv"
```

---

## Estrutura do projeto

```text
main.go        núcleo: AniList/MAL, Simulwatching, bandeja, loop de reprodução
window.go      janela principal nativa (walk): ciclo de vida, tray e ações da GUI
ui.go          a interface em si: paleta, tipografia, layout, desenho e hit-test
theme.go       tema escuro (páginas locais no navegador + chrome da janela)
mpv.go         cliente do JSON IPC + ciclo de vida do processo MPV
mpvdetect.go   busca do MPV e ajuste do mpv.conf
mpvpayload.go  payload do MPV embutido no executável
install.go     caminhos, config, guarda de instância única, migração, integração
associations.go registro do Kouki e do MPV no Windows (Start Menu, App Paths, associações)
bootstrap.go   ícone embutido
kouki.ico      ícone multi-resolução (7 tamanhos, 16–256)
kouki.syso     recurso do PE: ícone + versão 1.0.0 + manifest
kouki.exe.manifest
installer/     kouki.iss  (Inno Setup)
assets/mpv/    payload do MPV + portable_config (mpv.conf corrigido)
tools/         build.ps1, prepare-mpv.ps1, mkicon/
```

---

## Solução de problemas

**"Nenhum player conectado"** — o MPV não está com o canal IPC ligado. Abra com
o binário empacotado (`…\Kouki\mpv\mpv.exe`), que já traz
`input-ipc-server=\\.\pipe\mpv-pipe` no `mpv.conf`.

**A janela não abre** — se `walk` falhar ao criar a janela, o Kouki avisa e
continua funcionando pela bandeja.

**Presets do MPV sumiram** — o `mpv.conf` foi corrigido a partir do seu arquivo
original: ele escrevia várias opções na mesma linha e usava cabeçalhos sem `#`,
o que o MPV rejeita (o perfil `high-quality` **nunca existiu** por causa disso).
Todos os valores foram preservados; só a sintaxe mudou. Seu original não foi
alterado.

**Log** — `%APPDATA%\Kouki\kouki.log`

---

## Licenças

Kouki: MIT. O MPV empacotado é GPLv2+ de terceiros — ver
[`THIRD-PARTY-NOTICES.md`](./THIRD-PARTY-NOTICES.md).