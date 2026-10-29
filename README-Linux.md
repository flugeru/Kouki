# Kouki no Linux

Esta branch leva o Kouki ao Linux com a interface local no navegador, bandeja do sistema e MPV instalado no próprio sistema. O AppImage inclui o Kouki e seus recursos; ele não inclui o MPV.

## Instalação

Baixe o AppImage da execução mais recente do fluxo **Linux AppImage** no GitHub Actions. Escolha `x86_64` para PCs Intel/AMD ou `aarch64` para ARM64. Instale as dependências de execução:

```bash
# Ubuntu 22.04 / Debian compatível
sudo apt install mpv xdg-utils libayatana-appindicator3-1 libfuse2

# Ubuntu 24.04 ou mais recente
sudo apt install mpv xdg-utils libayatana-appindicator3-1 libfuse2t64
```

O pacote `libayatana-appindicator3-1` instala também as bibliotecas GTK necessárias para a bandeja. É preciso ter um navegador gráfico padrão configurado. O ícone de bandeja depende de suporte a AppIndicator no ambiente de desktop.

Depois de baixar:

```bash
chmod +x Kouki-*.AppImage
./Kouki-*.AppImage
```

Se o sistema não permitir FUSE, use `./Kouki-*.AppImage --appimage-extract-and-run` como alternativa.

## Reprodução e dados

O Kouki procura `mpv` no `PATH` e conversa com o player pelo JSON IPC em um socket Unix. Abra o MPV pelo Kouki para garantir que o canal de comunicação seja configurado. O MPV instalado pelo usuário continua separado do AppImage e pode receber atualizações pelo gerenciador de pacotes da distribuição.

Os dados do Kouki são gravados no diretório de configuração do usuário. O AppImage pode ficar em qualquer pasta e não exige instalação com privilégios de administrador.

## Compilação local

Requisitos: Linux, Go 1.26+, `gcc`, `pkg-config`, headers GTK 3 e Ayatana AppIndicator 3. Em Debian/Ubuntu:

```bash
sudo apt install gcc pkg-config libgtk-3-dev libayatana-appindicator3-dev desktop-file-utils curl
bash tools/build-appimage.sh
```

O resultado fica em `dist/Kouki-<versão>-<arquitetura>.AppImage`. O script obtém o `appimagetool` 1.9.1 quando ele não está instalado. Para usar uma cópia local, defina `APPIMAGETOOL=/caminho/para/appimagetool`.

## Limitações atuais

- A versão Linux usa a interface local aberta no navegador padrão. Ela acompanha as funções da aplicação Windows, com diferenças próprias do ambiente gráfico.
- O AppImage precisa do MPV, de um navegador gráfico, do `xdg-open` e das bibliotecas da bandeja no sistema. O suporte à bandeja varia com o ambiente de desktop.
- A branch ainda deve ser validada em uma sessão gráfica Linux real antes de uma publicação estável.
