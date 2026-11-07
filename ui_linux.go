//go:build linux

package main

import (
	"context"
	"embed"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed assets/linux-ui/index.html assets/linux-ui/app.css assets/linux-ui/app.js assets/icon/icon-512.png
var linuxUIAssets embed.FS

var linuxMPVPathCache struct {
	mu      sync.Mutex
	path    string
	checked time.Time
}

func linuxUIPlayerPath() string {
	linuxMPVPathCache.mu.Lock()
	defer linuxMPVPathCache.mu.Unlock()
	if time.Since(linuxMPVPathCache.checked) < 30*time.Second {
		return linuxMPVPathCache.path
	}
	linuxMPVPathCache.path = resolveMpvPath()
	linuxMPVPathCache.checked = time.Now()
	return linuxMPVPathCache.path
}

func invalidateLinuxUIPlayerPath() {
	linuxMPVPathCache.mu.Lock()
	linuxMPVPathCache.checked = time.Time{}
	linuxMPVPathCache.mu.Unlock()
}

type linuxUIState struct {
	Version      string          `json:"version"`
	Status       string          `json:"status"`
	StatusKind   string          `json:"statusKind"`
	Title        string          `json:"title"`
	Position     string          `json:"position"`
	Progress     float64         `json:"progress"`
	MPVPath      string          `json:"mpvPath"`
	AniListUser  string          `json:"aniListUser"`
	MALUser      string          `json:"malUser"`
	CanCorrect   bool            `json:"canCorrect"`
	SimulStatus  string          `json:"simulStatus"`
	SimulMode    string          `json:"simulMode"`
	Participants int             `json:"participants"`
	Toggles      map[string]bool `json:"toggles"`
	Messages     []linuxMessage  `json:"messages"`
}

func linuxUIStateSnapshot() linuxUIState {
	status, kind := "Desconectado", "muted"
	if st, ok := sharedMpvConn.Snapshot(); ok {
		switch {
		case st.Pause && st.Duration > 0:
			status, kind = "Pausado", "warning"
		case st.Duration > 0:
			status, kind = "Reproduzindo", "success"
		default:
			status, kind = "Conectado", "accent"
		}
	} else if sharedMpvConn.Connected() {
		status, kind = "Conectado", "accent"
	} else if guiPlayer.IsRunning() {
		status, kind = "Iniciando…", "warning"
	}
	progress := 0.0
	if snapshot, ok := sharedMpvConn.Snapshot(); ok && snapshot.Duration > 0 {
		progress = 100 * snapshot.TimePos / snapshot.Duration
	}
	path := linuxUIPlayerPath()
	if path == "" {
		path = "MPV não encontrado — instale o pacote mpv"
	}
	curTrack.mu.RLock()
	canCorrect := curTrack.Active && curTrack.SeriesKey != "" && curTrack.ParsedTitle != ""
	curTrack.mu.RUnlock()
	simul.mu.Lock()
	simulStatus, simulMode, participants := simul.status, string(simul.mode), simul.participants
	simul.mu.Unlock()
	linuxWindow.mu.RLock()
	messages := append([]linuxMessage(nil), linuxWindow.messages...)
	linuxWindow.mu.RUnlock()
	return linuxUIState{
		Version: appVersion, Status: status, StatusKind: kind,
		Title: nowPlayingTitle(), Position: nowPlayingMeta(), Progress: progress, MPVPath: path,
		AniListUser: storeAccount("anilist"), MALUser: storeAccount("mal"),
		CanCorrect: canCorrect, SimulStatus: simulStatus, SimulMode: simulMode,
		Participants: participants,
		Toggles: map[string]bool{
			"aniListTrack":  storeToggle("aniListTrack"),
			"fillerWarn":    storeToggle("fillerWarn"),
			"malTrack":      storeToggle("malTrack"),
			"simulJoinSync": storeToggle("simulJoinSync"),
			"runOnStartup":  storeToggle("runOnStartup"),
		},
		Messages: messages,
	}
}

func linuxWindowHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body, err := linuxUIAssets.ReadFile("assets/linux-ui/index.html")
		if err != nil {
			http.Error(w, "interface indisponível", 500)
			return
		}
		linuxWindow.mu.RLock()
		token := linuxWindow.token
		linuxWindow.mu.RUnlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		_, _ = w.Write([]byte(strings.ReplaceAll(string(body), "__KOUKI_TOKEN__", token)))
	})
	for path, assetType := range map[string]string{"/app.css": "text/css; charset=utf-8", "/app.js": "text/javascript; charset=utf-8", "/icon.png": "image/png"} {
		path, assetType := path, assetType
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			asset := "assets/linux-ui/" + strings.TrimPrefix(path, "/")
			if path == "/icon.png" {
				asset = "assets/icon/icon-512.png"
			}
			body, err := linuxUIAssets.ReadFile(asset)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", assetType)
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_, _ = w.Write(body)
		})
	}
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if !checkLinuxUIAccess(w, r, false) {
			return
		}
		writeJSON(w, 200, linuxUIStateSnapshot())
	})
	mux.HandleFunc("POST /api/action", func(w http.ResponseWriter, r *http.Request) {
		if !checkLinuxUIAccess(w, r, true) {
			return
		}
		var req struct {
			Action string `json:"action"`
			Host   string `json:"host"`
			Code   string `json:"code"`
		}
		if err := decodeLimitedJSON(r, &req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		var err error
		switch req.Action {
		case "simul-host":
			code := strings.TrimSpace(req.Code)
			if !validateJoinCode(code) {
				err = errors.New("o código deve ter de 3 a 18 letras ou números")
			} else {
				simul.mu.Lock()
				simul.code = code
				simul.mu.Unlock()
				go simulStartHost(context.Background())
			}
		case "simul-join":
			host, code := strings.TrimSpace(req.Host), strings.TrimSpace(req.Code)
			if host == "" || !validateJoinCode(code) {
				err = errors.New("informe endereço e código da sala")
			} else {
				go simulStartJoin(context.Background(), host, code)
			}
		default:
			err = guiAction(req.Action)
		}
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/toggle", func(w http.ResponseWriter, r *http.Request) {
		if !checkLinuxUIAccess(w, r, true) {
			return
		}
		var req struct {
			Key   string `json:"key"`
			Value bool   `json:"value"`
		}
		if err := decodeLimitedJSON(r, &req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := guiSetToggle(req.Key, req.Value); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		linuxWindow.mu.RLock()
		base := linuxWindow.url
		linuxWindow.mu.RUnlock()
		u, _ := url.Parse(base)
		if u == nil || r.Host != u.Host {
			http.Error(w, "host inválido", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func checkLinuxUIAccess(w http.ResponseWriter, r *http.Request, mutation bool) bool {
	linuxWindow.mu.RLock()
	token, base := linuxWindow.token, linuxWindow.url
	linuxWindow.mu.RUnlock()
	if token == "" || r.Header.Get("X-Kouki-Token") != token {
		http.Error(w, "acesso negado", http.StatusForbidden)
		return false
	}
	if mutation {
		origin := strings.TrimSuffix(base, "/")
		if r.Header.Get("Origin") != origin {
			http.Error(w, "origem inválida", http.StatusForbidden)
			return false
		}
	}
	return true
}
