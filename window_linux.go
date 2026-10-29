//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The Linux window is a loopback-only dashboard. It uses the user's browser,
// which keeps the AppImage independent of GTK, WebKit and a particular desktop.
var guiPlayer = &mpvPlayer{}
var quitSignal = make(chan struct{})
var quitOnce sync.Once

var linuxWindow struct {
	mu       sync.RWMutex
	url      string
	token    string
	server   *http.Server
	messages []linuxMessage
	nextID   int64
}

type linuxMessage struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Kind  string `json:"kind"`
}

func runMainWindow() {
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		logAppend("interface: falha ao gerar token: ", err.Error())
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logAppend("interface: falha ao abrir porta local: ", err.Error())
		return
	}
	url := "http://" + listener.Addr().String() + "/"
	linuxWindow.mu.Lock()
	linuxWindow.url = url
	linuxWindow.token = hex.EncodeToString(tokenBytes)
	linuxWindow.server = &http.Server{Handler: linuxWindowHandler()}
	server := linuxWindow.server
	linuxWindow.mu.Unlock()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logAppend("interface: servidor local: ", err.Error())
			requestQuit()
		}
	}()
	guiScanMpvOnStartup()
	if !startHidden {
		showMainWindow()
	}
	<-quitSignal
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	if guiPlayer.IsRunning() {
		guiPlayer.Stop(mpvPipeName(), 3*time.Second)
	}
}

func showMainWindow() {
	linuxWindow.mu.RLock()
	url := linuxWindow.url
	linuxWindow.mu.RUnlock()
	if url == "" {
		return
	}
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		logAppend("interface: xdg-open: ", err.Error())
		fmt.Fprintln(os.Stderr, "Abra o Kouki em", url)
	}
}

// A browser tab cannot be hidden by an HTTP response. The dashboard stays
// available, and the user can close the tab while playback monitoring runs.
func hideMainWindow() {}

func requestQuit()    { quitOnce.Do(func() { close(quitSignal) }) }
func refreshGui()     {}
func refreshGuiSafe() {}

func guiMessage(title, text string, style uint32) {
	kind := "info"
	if style&mbIconError != 0 {
		kind = "error"
	} else if style&mbIconWarning != 0 {
		kind = "warning"
	}
	linuxWindow.mu.Lock()
	linuxWindow.nextID++
	linuxWindow.messages = append(linuxWindow.messages, linuxMessage{linuxWindow.nextID, title, text, kind})
	if len(linuxWindow.messages) > 30 {
		linuxWindow.messages = linuxWindow.messages[len(linuxWindow.messages)-30:]
	}
	linuxWindow.mu.Unlock()
	logAppend("interface: ", title, ": ", text)
}

func guiError(title, text string) { guiMessage(title, text, mbOK|mbIconError) }

func storeToggle(key string) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	switch key {
	case "aniListTrack":
		return store.AniListTracking
	case "fillerWarn":
		return store.WarnFiller
	case "malTrack":
		return store.MALTracking
	case "simulJoinSync":
		return store.SimulJoinSync
	case "runOnStartup":
		return store.RunOnStartup
	}
	return false
}

func storeAccount(service string) string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if service == "mal" {
		return strings.TrimSpace(store.MALUsername)
	}
	return strings.TrimSpace(store.Username)
}

func guiSetToggle(key string, value bool) error {
	if key == "runOnStartup" {
		if err := setLinuxAutostart(value); err != nil {
			return err
		}
	}
	store.mu.Lock()
	switch key {
	case "aniListTrack":
		store.AniListTracking = value
	case "fillerWarn":
		store.WarnFiller = value
	case "malTrack":
		store.MALTracking = value
	case "simulJoinSync":
		store.SimulJoinSync = value
	case "runOnStartup":
		store.RunOnStartup = value
	default:
		store.mu.Unlock()
		return errors.New("preferência desconhecida")
	}
	store.mu.Unlock()
	store.Save()
	return nil
}

func setLinuxAutostart(enabled bool) error {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		base = filepath.Join(home, ".config")
	}
	path := filepath.Join(base, "autostart", "kouki.desktop")
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := launcherExecutable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Desktop Entry quoting uses double quotes, with backslash escaping inside.
	quote := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(exe)
	content := "[Desktop Entry]\nType=Application\nName=Kouki\nComment=Contas, reprodução e progresso\nExec=\"" + quote + "\" --tray\nIcon=kouki\nTerminal=false\nCategories=AudioVideo;Player;\nX-GNOME-Autostart-enabled=true\n"
	return os.WriteFile(path, []byte(content), 0o600)
}

// AppImage extracts or mounts its payload at an ephemeral path. Desktop launchers
// must point to the outer AppImage, which the runtime exposes as APPIMAGE.
func launcherExecutable() (string, error) {
	if image := strings.TrimSpace(os.Getenv("APPIMAGE")); image != "" {
		return filepath.Abs(image)
	}
	return os.Executable()
}

func openFolder(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	if err := exec.Command("xdg-open", dir).Start(); err != nil {
		guiError(appName, "Não foi possível abrir a pasta: "+err.Error())
	}
}

func showAboutDialog() {
	guiMessage("Sobre o Kouki", "Kouki v"+appVersion+" para Linux\n\nMPV do sistema via socket Unix.\nDados: "+appDataDir()+"\nCódigo: https://github.com/flugeru/Kouki", mbOK|mbIconInfo)
}

func guiScanMpvOnStartup() {
	go func() {
		if exe := resolveMpvPath(); exe != "" {
			if err := EnsureMpvIpcConfig(exe); err != nil {
				logAppend("mpv: configuração IPC: ", err.Error())
			}
		}
	}()
}

func guiDetectMpv() {
	list := FindMpvCandidates()
	invalidateLinuxUIPlayerPath()
	if len(list) == 0 {
		guiMessage(appName, "MPV não encontrado. Instale o pacote mpv da sua distribuição e tente novamente.", mbOK|mbIconWarning)
		return
	}
	var lines []string
	for _, candidate := range list {
		lines = append(lines, candidate.Path)
	}
	guiMessage("MPV encontrado", strings.Join(lines, "\n"), mbOK|mbIconInfo)
}

func guiAction(name string) error {
	cfg := loadConfig()
	switch name {
	case "ani-login":
		go func() {
			if err := oauthLogin(context.Background()); err != nil {
				guiError("Kouki: AniList", err.Error())
			}
		}()
	case "ani-logout":
		store.ClearAniList()
	case "mal-login":
		if strings.TrimSpace(cfg.MALClientID) == "" || strings.TrimSpace(cfg.MALClientSecret) == "" {
			return errors.New("configure MAL_CLIENT_ID e MAL_CLIENT_SECRET para entrar no MyAnimeList")
		}
		go func() {
			if err := malOAuthLogin(context.Background(), cfg); err != nil {
				guiError("Kouki: MyAnimeList", err.Error())
			}
			store.Load()
		}()
	case "mal-logout":
		store.ClearMAL()
	case "ani-correct", "mal-correct":
		curTrack.mu.RLock()
		seriesKey, query := curTrack.SeriesKey, curTrack.ParsedTitle
		active := curTrack.Active && curTrack.LastFileKey != ""
		curTrack.mu.RUnlock()
		if !active || seriesKey == "" || query == "" {
			return errors.New("nenhum arquivo em reprodução")
		}
		if name == "ani-correct" {
			return startAniListSelector(query, cfg.UserAgent, func(id int) {
				overrides.Set(seriesKey, id)
				select {
				case resolveCh <- resolveRequest{SeriesKey: seriesKey, MediaID: id}:
				default:
				}
			})
		}
		return startMALSelector(query, cfg.MALClientID, cfg.UserAgent, func(id int) {
			malOverrides.Set(seriesKey, id)
			select {
			case resolveMALCh <- resolveMALRequest{SeriesKey: seriesKey, AnimeID: id}:
			default:
			}
		})
	case "simul-stop":
		simulStop()
	case "mpv-launch":
		exe := resolveMpvPath()
		if exe == "" {
			return errors.New("MPV não encontrado; instale o pacote mpv da sua distribuição")
		}
		if err := guiPlayer.Launch(exe, mpvLaunchOptions{Pipe: mpvPipeName()}); err != nil {
			return err
		}
	case "mpv-stop":
		go func() {
			if conn := sharedMpvConn.get(); conn != nil {
				_ = conn.setCommand("quit")
				_ = conn.Close()
				sharedMpvConn.set(nil)
			}
			guiPlayer.Stop(mpvPipeName(), 3*time.Second)
		}()
	case "mpv-detect":
		go guiDetectMpv()
	case "mpv-toggle-pause", "mpv-play", "mpv-pause", "mpv-next", "mpv-previous", "mpv-fullscreen", "mpv-volume-up", "mpv-volume-down":
		return mpvDispatch(sharedMpvConn.get(), strings.TrimPrefix(name, "mpv-"))
	case "open-install-folder":
		exe, err := launcherExecutable()
		if err != nil {
			return err
		}
		openFolder(filepath.Dir(exe))
	case "open-config-folder":
		_ = os.MkdirAll(appDataDir(), 0o700)
		openFolder(appDataDir())
	case "open-mpv-folder":
		exe := resolveMpvPath()
		if exe == "" {
			return errors.New("MPV não encontrado")
		}
		openFolder(filepath.Dir(exe))
	case "about":
		showAboutDialog()
	case "quit":
		requestQuit()
	default:
		return errors.New("ação desconhecida")
	}
	return nil
}

func nowPlayingTitle() string {
	if st, ok := sharedMpvConn.Snapshot(); ok {
		if title := firstNonEmpty(st.MediaTitle, st.FileName, st.Path); title != "" {
			return title
		}
	}
	if sharedMpvConn.Connected() {
		return "Player conectado — nenhum arquivo aberto"
	}
	if guiPlayer.IsRunning() {
		return "Player iniciado — aguardando o canal"
	}
	return "Nenhum player conectado"
}

func nowPlayingMeta() string {
	st, ok := sharedMpvConn.Snapshot()
	if !ok || st.Duration <= 0 {
		return ""
	}
	return formatDuration(st.TimePos) + " / " + formatDuration(st.Duration)
}

func formatDuration(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	h, m, s := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeLimitedJSON(r *http.Request, out any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("tipo de conteúdo inválido")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, 2048)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("JSON extra")
	}
	return nil
}
