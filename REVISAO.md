# Revisão do Kouki

> **Leia primeiro:** esta revisão foi feita **sem compilador Go e sem rede**. O código
> foi lido por inteiro e as correções foram conferidas por inspeção (chaves, imports,
> nomes duplicados e a geometria do desenho de cantos, simulada à parte), mas **nada foi
> compilado nem executado**. Rode antes de confiar:
>
> ```powershell
> go build ./... ; go vet ./...
> ```
>
> Se algo não compilar, o erro deve estar num dos trechos listados abaixo — me mande a saída.

---

## 1. Erros corrigidos

### Interface (`ui.go`)
| Problema | Efeito | Correção |
|---|---|---|
| `loadUIFonts` terminava atribuindo `fBody` a **todas** as fontes | Nome do app, títulos dos cards e botões saíam em 9 pt normal; a hierarquia tipográfica não existia | Só cai para `fBody` a fonte que não pôde ser criada |
| "Conectado" era inalcançável | MPV aberto e sem arquivo mostrava "Iniciando…" para sempre (ou "Desconectado" com o canal ativo) | Estado calculado também pela conexão, não só pelo snapshot |
| Cabeçalho ficava fixo enquanto o resto rolava | Em tela baixa os cards passavam por cima do logo e da pílula; um repaint "teletransportava" o cabeçalho | Cabeçalho rola junto |
| Thread da janela falava com o MPV | 3 leituras (7 idas e voltas cada) por tick **na thread da UI**; um mpv lento travava a janela | O loop de reprodução publica o estado; a janela só lê a cópia |
| Hover grudado ao sair da janela | Botão ficava realçado | Detecção de saída do ponteiro (só enquanto algo está sob o mouse) |
| Anel de foco após clique do mouse | Parecia um estado "selecionado" | Anel só aparece após navegar pelo teclado |
| Botões sempre ativos | "Pausar" sem MPV, "Selecionar anime" sem nada tocando abriam caixas de erro | Botões desabilitados quando a ação não faz sentido; "Entrar" vira "Reconectar" com conta |
| Cantos serrilhados e raios pela metade | GDI `RoundRect` usa o *diâmetro*; cards/botões/pílula ficavam quadradões e sem antialias | Cantos antialiased (retângulos + 4 quadrantes em cache) e raios reais |
| Pílula de status com 196 px | Quase metade vazia | Largura medida a partir do texto mais largo |
| Tab não rolava até o controle; setas rolavam uma página | — | `ensureVisible`; setas rolam 48 px, PgUp/PgDn uma página |
| Scrollbar e espaços com pixels fixos | Desproporcional em DPI > 100% | `uiMetric` |
| `uiLineHeight` medindo a cada texto | Trabalho GDI desnecessário por paint | Cache por fonte |

### Janela e ciclo de vida (`window.go`, `install.go`, `main.go`)
- **"Sair" do AniList apagava tudo** (`store.Clear()`: login do MAL, preferências, `auth.json`). Agora só desconecta o AniList (`ClearAniList`), na janela **e** na bandeja.
- **Migração do Koushin rodava depois do `store.Load()`**: na primeira execução o login migrado não era lido e o primeiro salvamento podia sobrescrevê-lo. Migração agora vem antes.
- **Minimizar não escondia a janela**, ao contrário do README. Agora esconde (e restaurar pela bandeja funciona de minimizado, com foco).
- **Abrir o Kouki de novo não fazia nada visível**; além disso, qualquer programa usando a porta 45222 impedia o Kouki de abrir, em silêncio. Agora o guarda é um mutex nomeado e a porta virou "campainha" que traz a janela para a frente.
- O instalador declarava `AppMutex=KoukiSingleInstanceMutex`, mas **nada criava esse mutex**; agora o app cria.
- **Checkbox "iniciar com o Windows" do instalador não fazia nada** (e o app apagava a entrada ao iniciar). Agora grava `Kouki.exe --tray` e o app adota a entrada; sobe direto na bandeja.
- "Encerrar MPV" sem conexão usava só o *nome* do pipe (`mpv-pipe`) em vez do caminho completo → nunca conectava.
- Encerrar o Kouki, "Encerrar MPV", "Procurar MPV" e o registro do MPV rodavam **na thread da UI** (segundos de congelamento). Agora em goroutines.
- "Entrar" do AniList duas vezes abria dois listeners na mesma porta; agora o segundo clique cancela o primeiro (como já era no MAL).
- `refreshGui()` chamado de goroutine (MAL) → `refreshGuiSafe()`; `guiIsQuitting` e `guiFoundMpv` eram acessados sem sincronização.
- Alternar "iniciar com o Windows" que falha agora desfaz o switch.
- "Procurar MPV" só listava, e a mensagem mandava usar um "menu de contexto" que não existe mais. Agora adota o primeiro candidato.

### MPV (`mpv.go`, `mpvdetect.go`, `mpvpayload.go`)
- `notFound` comparava texto em **português** do Windows; agora `errors.Is`.
- `DialPipe(…, nil)` podia esperar para sempre num pipe ocupado; agora tem timeout.
- Corrida de dados em `cmd.ProcessState` (lido enquanto `cmd.Wait` escrevia).
- `strings.Contains(err.Error(), "no file playing")` → `errors.Is(err, errNoFilePlaying)`.
- `EnsureMpvIpcConfig` achava que `#input-ipc-server=…` (comentado) já estava configurado, e calculava `MPV_PIPE` mas escrevia sempre o pipe fixo.
- `sortCandidates` ignorava a cópia do Kouki em `%LOCALAPPDATA%\Programs\mpv` e a escolha do usuário.
- Instalar o payload num build sem `mpv.exe` embutido deixava uma pasta pela metade e falhava com mensagem confusa; agora diz a causa.

### Segurança e robustez (`main.go`)
- `/submit` do login do AniList aceitava qualquer POST local (mesmo `text/plain` de outro site): um site podia injetar um token alheio. Agora exige `application/json`, limita o corpo e não bloqueia em envios repetidos.
- `auth.json` e `overrides*.json` eram gravados com `os.WriteFile` (trunca antes de escrever): queda no meio = **tokens perdidos**. Agora escrita atômica (temp + rename) com mutex.
- `io.ReadAll(resp.Body)` sem limite em 10 lugares → limitado a 8 MB.
- `kouki.log` e `mpv.log` cresciam sem limite → rotação (1 MB / 2 MB).
- `ensureKoukiStartMenu` podia dar *nil dereference* (`err.Error()` com `err == nil`).
- Mensagens em inglês num app "todo em português": aviso de filler, convite do Simulwatching, erros de login.
- `fmt.Println` num app `-H=windowsgui` (vai para lugar nenhum) → `logAppend`.
- `store.WarnFiller` lido sem lock no loop.

### Instalador (`installer/kouki.iss`)
- Removidas a seção `[Dirs]` (mexia na ACL de `C:\ProgramData` numa instalação sem admin) e a limpeza do `EBWebView` (resto da época do WebView2).

---

## 2. Precisa de uma decisão sua (não mexi)

1. **Segredo do MAL no código-fonte.** `loadConfig()` (main.go, ~linha 1695) embute um `client_secret` como fallback — contradiz o comentário logo acima ("sem segredo, o login fica desativado") e o repositório é público. Qualquer um extrai de qualquer jeito de um binário, então: **revogue/gere outro no MAL**, tire do código e use só PKCE se o tipo de app permitir. Se a credencial for do autor do Koushin (não sua), o motivo é ainda maior. Não removi porque quebraria o login do MAL para todo mundo.
2. **MPV empacotado duas vezes.** `mpvpayload.go` embute `assets/mpv` no `.exe` (~127 MB) e o instalador copia a mesma pasta para `{app}\mpv`; `bootstrap.go` e `window.go` ainda dizem que o payload *não* é embutido. Escolha um dos dois caminhos.
3. **`mpv.conf` com `gpu-api=vulkan` fixo** (e deband/ewa pesados): em máquina sem Vulkan o vídeo não abre. `auto` é mais seguro para distribuir.
4. **Fontes e DPI.** `loadUIFonts` multiplica o tamanho por `dpi/96`, mas (pelo que conheço da versão do walk usada) `walk.Font` já escala pelo DPI do canvas — o texto poderia ficar ~1,5× maior que as caixas a 125%/150%. **Confira nessas escalas**; se estiver grande, remova o `* dpi / 96` em `scale()`.
5. **Simulwatching**: o anfitrião escuta em `0.0.0.0` e o código tem 3–18 caracteres sem limite de tentativas nem comparação em tempo constante. Sugiro mínimo de 6 caracteres e bloqueio por IP após N falhas.
6. **`setMpvAsDefaultViaCOM`** não faz efeito no Windows 10/11 (o padrão é protegido por hash) e o objeto COM provavelmente nem expõe `SetAppAsDefaultAll` via PowerShell. Além disso registra 33 extensões, incluindo `.ts` (TypeScript).
7. `assets/.../elements/Menu.zip` é um `Menu.lua` antigo (60 271 bytes) que vai embutido e instalado sem uso. Se for backup seu, tire do `assets`.

---

## 3. Melhorias sugeridas (para ver outra hora)

**Interface**
- As strings `tip:` dos botões **não aparecem em lugar nenhum** (o tooltip nativo foi desligado por causa de flicker). Desenhar um tooltip próprio no canvas ou apagar o código morto.
- Reagir à troca de DPI/monitor (`WM_DPICHANGED`): fontes e métricas são carregadas uma vez.
- Acessibilidade (UI Automation/leitor de tela) e alto contraste: hoje inexistentes por causa do canvas único.
- Ícones nos botões principais (Segoe MDL2 já está carregada); estado "ocupado" nos botões que abrem o navegador.
- Mostrar o *episódio/anime resolvido* (AniList/MAL) no card de reprodução, não só o nome do arquivo.
- Persistir o MPV escolhido (hoje `selectedMpvPath` vale só na sessão).

**Código**
- `main.go` tem 6 300 linhas: separar em `anilist.go`, `mal.go`, `simul.go`, `discord.go`, `tray.go`.
- **Nenhum teste**. `pickBest`, `extractStreamFilename`, `pickEpisode`, `releaseSignatureForZeroBased` e `configHasIPC` são puras e ótimas candidatas.
- A versão `1.0.1` está repetida em 5 lugares (`install.go`, `.iss`, `versioninfo.json`, README, THIRD-PARTY); gerar a partir de uma só.
- Tokens em texto puro em `%APPDATA%`: proteger com DPAPI (`CryptProtectData`).
- O README ainda descreve o `walk` como "janela principal nativa" e fala em tooltip; atualizar junto com o item acima.
- Mensagens de log sem acento ("apos", "nao") e algumas strings de erro do MAL ainda em inglês.
- `go.mod` fixa `go 1.26.0` e versões de dependência que não consegui validar offline — confira com `go mod tidy`.

---

## 4. Roteiro de teste na sua máquina

1. `go build` e `go vet` sem erros.
2. Abrir: fontes com hierarquia (nome grande em negrito, títulos de card em negrito); cantos suaves.
3. Sem MPV aberto: pílula "Desconectado", **Pausar** e **Encerrar** apagados. **Iniciar MPV** → "Iniciando…" → **"Conectado"** (sem arquivo) → "Reproduzindo".
4. Mover o mouse para fora da janela: nenhum botão fica realçado. Clicar num botão: sem anel de foco; Tab: anel aparece.
5. Janela baixa (arraste a borda): cabeçalho rola junto, sem sobreposição; Tab rola até o controle.
6. Logar nos dois serviços, **Sair** só do AniList: o MAL continua logado.
7. Minimizar → some da barra; bandeja → "Abrir o Kouki" → volta com foco. Abrir o atalho de novo com ele na bandeja → janela aparece.
8. Instalador com "iniciar com o Windows" marcado → reiniciar → sobe só na bandeja.
9. 125% e 150% de escala: nenhum texto cortado nem exagerado (ver item 4 da seção 2).
