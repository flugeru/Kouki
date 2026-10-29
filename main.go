package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	habari "github.com/5rahim/habari"
	"github.com/Microsoft/go-winio"
	"github.com/getlantern/systray"
	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	natpmp "github.com/jackpal/go-nat-pmp"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type simulMode string

const (
	simulModeOff  simulMode = "off"
	simulModeHost simulMode = "host"
	simulModeJoin simulMode = "join"
)

type simulMessage struct {
	Type string          `json:"type"`
	TS   int64           `json:"ts"`
	Data json.RawMessage `json:"data,omitempty"`
}

type simulHello struct {
	Code string `json:"code"`
}

type simulParticipants struct {
	Participants int `json:"participants"`
}

type simulHelloOK struct {
	Role      string `json:"role"`
	HostName  string `json:"host_name,omitempty"`
	HostVer   string `json:"host_ver,omitempty"`
	Joiners   int    `json:"joiners,omitempty"`
	InviteStr string `json:"invite,omitempty"`
}

type simulState struct {
	Anime      string  `json:"anime"`
	Episode    string  `json:"episode"`
	AniID      int     `json:"ani_id"`
	TotalEps   int     `json:"total_eps"`
	CoverURL   string  `json:"cover_url"`
	Rewatching bool    `json:"rewatching"`
	Paused     bool    `json:"paused"`
	Pos        float64 `json:"pos"`
	Dur        float64 `json:"dur"`
	Ready      bool    `json:"ready"`
	UpdatedAt  int64   `json:"updated_at"`
	HostFile   string  `json:"host_file,omitempty"`
	HostSeason int     `json:"host_season,omitempty"`
}

type simulSyncEvent struct {
	AniID     int   `json:"ani_id"`
	Episode   int   `json:"episode"`
	TotalEps  int   `json:"total_eps"`
	AtUnixSec int64 `json:"at_unix_sec"`
}

type simulConfig struct {
	Port         int
	AutoPortMap  bool
	JoinerSyncAL bool
}

type simulRuntime struct {
	mu sync.Mutex

	mode simulMode

	ln         net.Listener
	code       string
	clients    map[net.Conn]struct{}
	inviteText string
	portMapOff func()
	hostState  simulState

	conn      net.Conn
	lastState simulState
	status    string

	participants int

	cfg simulConfig
}

var simul = simulRuntime{mode: simulModeOff, status: "Desligado"}

func simulLoadSettingsFromStore() {
	store.mu.RLock()
	port := store.SimulPort
	auto := store.SimulAutoUPnP
	joinSync := store.SimulJoinSync
	store.mu.RUnlock()
	if !validateSimulPort(port) {
		port = defaultSimulPort()
	}
	simul.mu.Lock()
	simul.cfg = simulConfig{Port: port, AutoPortMap: auto, JoinerSyncAL: joinSync}
	simul.mu.Unlock()
}

func simulSetStatusLocked(status string) {
	simul.status = status
	if menuSimulStatus != nil {
		menuSimulStatus.SetTitle("Status: " + status)
	}
}

func simulSetParticipantsLocked(n int) {
	if n < 0 {
		n = 0
	}
	simul.participants = n
}

func simulRefreshTray() {
	simul.mu.Lock()
	mode := simul.mode
	invite := simul.inviteText
	participants := simul.participants
	simul.mu.Unlock()

	if menuSimulStatus != nil {
		status := "Desligado"
		switch mode {
		case simulModeHost:
			if participants <= 0 {
				participants = 1
			}
			status = fmt.Sprintf("Anfitrião (%d participantes)", participants)
		case simulModeJoin:
			if participants <= 0 {
				participants = 1
			}
			status = fmt.Sprintf("Conectado (%d participantes)", participants)
		default:
			status = "Desligado"
		}
		menuSimulStatus.SetTitle("Status: " + status)
		if invite != "" {
			menuSimulStatus.SetTooltip(invite)
		}
	}
	if menuSimulHost != nil {
		if mode == simulModeOff {
			menuSimulHost.Enable()
		} else {
			menuSimulHost.Disable()
		}
	}
	if menuSimulJoin != nil {
		if mode == simulModeOff {
			menuSimulJoin.Enable()
		} else {
			menuSimulJoin.Disable()
		}
	}
	if menuSimulStop != nil {
		if mode == simulModeOff {
			menuSimulStop.Disable()
		} else {
			menuSimulStop.Enable()
		}
	}
	if menuSimulJoinSync != nil {
		if mode == simulModeJoin {
			menuSimulJoinSync.Enable()
		} else {
			menuSimulJoinSync.Disable()
		}
		store.mu.RLock()
		on := store.SimulJoinSync
		store.mu.RUnlock()
		if on {
			menuSimulJoinSync.Check()
		} else {
			menuSimulJoinSync.Uncheck()
		}
	}
}

func simulStopLocked() {
	if simul.portMapOff != nil {
		simul.portMapOff()
		simul.portMapOff = nil
	}
	if simul.conn != nil {
		_ = simul.conn.Close()
		simul.conn = nil
	}
	if simul.ln != nil {
		_ = simul.ln.Close()
		simul.ln = nil
	}
	for c := range simul.clients {
		_ = c.Close()
	}
	simul.clients = nil
	simul.code = ""
	simul.inviteText = ""
	simul.hostState = simulState{}
	simul.lastState = simulState{}
	simul.mode = simulModeOff
	simulSetParticipantsLocked(0)
	simulSetStatusLocked("Desligado")
}

func simulStop() {
	simul.mu.Lock()
	simulStopLocked()
	simul.mu.Unlock()
	simulRefreshTray()
}

func simulBroadcastToClients(typ string, payload any) {
	simul.mu.Lock()
	if simul.mode != simulModeHost || len(simul.clients) == 0 {
		simul.mu.Unlock()
		return
	}
	clients := make([]net.Conn, 0, len(simul.clients))
	for c := range simul.clients {
		clients = append(clients, c)
	}
	simul.mu.Unlock()

	for _, c := range clients {
		_ = c.SetWriteDeadline(time.Now().Add(800 * time.Millisecond))
		if err := writeSimulMsg(c, typ, payload); err != nil {
			simul.mu.Lock()
			if simul.clients != nil {
				delete(simul.clients, c)
			}
			simul.mu.Unlock()
			_ = c.Close()
		}
	}
}

func simulBroadcastState(st simulState) {
	simulBroadcastToClients("state", st)
}

func simulBroadcastSync(ev simulSyncEvent) {
	simulBroadcastToClients("sync", ev)
}

func simulHandleClientConn(ctx context.Context, c net.Conn) {
	defer func() {
		simul.mu.Lock()
		if simul.clients != nil {
			delete(simul.clients, c)
		}

		joiners := 0
		if simul.mode == simulModeHost && simul.clients != nil {
			joiners = len(simul.clients)
		}
		simulSetParticipantsLocked(1 + joiners)
		pc := simul.participants
		simul.mu.Unlock()
		simulRefreshTray()
		if pc > 0 {
			simulBroadcastToClients("participants", simulParticipants{Participants: pc})
		}
		_ = c.Close()
	}()

	dec := json.NewDecoder(c)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := readSimulMsg(dec)
	if err != nil || m.Type != "hello" {
		return
	}
	var hello simulHello
	_ = json.Unmarshal(m.Data, &hello)
	if !validateJoinCode(hello.Code) {
		_ = writeSimulMsg(c, "error", map[string]any{"error": "invalid code"})
		return
	}

	simul.mu.Lock()
	code := simul.code
	if simul.mode != simulModeHost || code == "" || hello.Code != code {
		simul.mu.Unlock()
		_ = writeSimulMsg(c, "error", map[string]any{"error": "wrong code"})
		return
	}
	if simul.clients == nil {
		simul.clients = make(map[net.Conn]struct{})
	}
	joinersBefore := len(simul.clients)
	joinersAfter := joinersBefore + 1
	invite := simul.inviteText
	pc := 1 + joinersAfter
	st := simul.hostState
	simul.mu.Unlock()

	_ = c.SetReadDeadline(time.Time{})
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := writeSimulMsg(c, "hello_ok", simulHelloOK{Role: "joiner", HostName: "Kouki", HostVer: appVersion, Joiners: joinersAfter, InviteStr: invite}); err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = writeSimulMsg(c, "participants", simulParticipants{Participants: pc})
	if strings.TrimSpace(st.Anime) != "" {
		_ = c.SetWriteDeadline(time.Now().Add(800 * time.Millisecond))
		_ = writeSimulMsg(c, "state", st)
	}

	simul.mu.Lock()
	if simul.mode != simulModeHost || simul.code == "" || simul.code != hello.Code {
		simul.mu.Unlock()
		return
	}
	if simul.clients == nil {
		simul.clients = make(map[net.Conn]struct{})
	}
	simul.clients[c] = struct{}{}
	joiners := len(simul.clients)
	simulSetParticipantsLocked(1 + joiners)
	pc2 := simul.participants
	simul.mu.Unlock()
	simulRefreshTray()
	if pc2 > 0 {
		simulBroadcastToClients("participants", simulParticipants{Participants: pc2})
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		msg, err := readSimulMsg(dec)
		if err != nil {
			return
		}
		if msg.Type == "ping" {
			_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = writeSimulMsg(c, "pong", nil)
		}
	}
}

func simulStartHost(ctx context.Context) {
	simulLoadSettingsFromStore()

	simul.mu.Lock()
	if simul.mode != simulModeOff {
		simul.mu.Unlock()
		return
	}
	if !validateJoinCode(simul.code) {
		simul.mu.Unlock()
		messageBox("Kouki: Simulwatching", "Código do anfitrião inválido. Escolha um código com 3–18 letras/números.", mbOK|mbIconError)
		return
	}
	port := simul.cfg.Port
	autoUPnP := simul.cfg.AutoPortMap
	simul.mode = simulModeHost

	simul.clients = make(map[net.Conn]struct{})
	simulSetParticipantsLocked(1)
	simulSetStatusLocked("Anfitrião")
	simul.mu.Unlock()
	simulRefreshTray()

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		simul.mu.Lock()
		simulStopLocked()
		simulSetStatusLocked("Desligado")
		simul.mu.Unlock()
		simulRefreshTray()
		messageBox("Kouki: Simulwatching", "Não foi possível iniciar a sessão de Simulwatching:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	publicIP0 := ""
	{
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		publicIP0, _ = detectPublicIP(cctx)
		cancel()
	}

	simul.mu.Lock()
	simul.ln = ln
	code := simul.code
	invite := fmt.Sprintf("Convite: %s:%d  Código: %s", firstNonEmpty(publicIP0, "<seu-ip-público>"), port, code)
	simul.inviteText = invite
	simulSetStatusLocked("Anfitrião")
	simul.mu.Unlock()
	simulRefreshTray()

	messageBox("Kouki: anfitrião do Simulwatching", invite+"\n\nSe seu amigo não conseguir conectar, pode ser necessário encaminhar a porta TCP "+strconv.Itoa(port)+" no roteador para este PC.", mbOK|mbIconInfo)

	go func() {
		defer logRecoveredPanic("simulStartHost.background")
		var cleanup func()
		publicIP := publicIP0
		if autoUPnP {
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			cl, ext, perr := tryPortMapUPnP(cctx, port, "Kouki Simulwatching", 3600)
			cancel()
			if perr == nil {
				cleanup = cl
				publicIP = strings.TrimSpace(ext)
			} else {
				cctx2, cancel2 := context.WithTimeout(ctx, 6*time.Second)
				cl2, ext2, perr2 := tryPortMapNATPMP(cctx2, port, 3600)
				cancel2()
				if perr2 == nil {
					cleanup = cl2
					publicIP = strings.TrimSpace(ext2)
				} else {
					logAppend("simul: port mapping failed: ", perr.Error(), " / ", perr2.Error())
				}
			}
		}
		if publicIP == "" {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			publicIP, _ = detectPublicIP(cctx)
			cancel()
		}

		simul.mu.Lock()
		stillHosting := simul.mode == simulModeHost && simul.ln == ln
		if stillHosting {
			if cleanup != nil {
				if simul.portMapOff != nil {
					simul.portMapOff()
				}
				simul.portMapOff = cleanup
			}
			invite2 := fmt.Sprintf("Convite: %s:%d  Código: %s", firstNonEmpty(publicIP, "<seu-ip-público>"), port, code)
			simul.inviteText = invite2
			simulSetStatusLocked("Anfitrião")
		}
		simul.mu.Unlock()
		if stillHosting {
			simulRefreshTray()
		} else if cleanup != nil {
			cleanup()
		}
	}()

	go func() {
		if !autoUPnP {
			return
		}
		t := time.NewTicker(25 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				cl, _, perr := tryPortMapUPnP(cctx, port, "Kouki Simulwatching", 3600)
				cancel()
				if perr == nil && cl != nil {
					simul.mu.Lock()
					if simul.portMapOff != nil {
						simul.portMapOff()
					}
					simul.portMapOff = cl
					simul.mu.Unlock()
				}
			}
		}
	}()

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go simulHandleClientConn(ctx, conn)
		}
	}()
}

func simulApplyJoinStateToUI(cfg Config, mgr *discordManager) {
	simul.mu.Lock()
	if simul.mode != simulModeJoin {
		simul.mu.Unlock()
		return
	}
	st := simul.lastState
	status := simul.status
	simul.mu.Unlock()
	_ = status

	pos := st.Pos
	if !st.Paused && st.Dur > 0 && st.UpdatedAt > 0 {
		delta := float64(time.Now().UnixNano()-st.UpdatedAt) / float64(time.Second)
		pos = pos + delta
		if pos > st.Dur {
			pos = st.Dur
		}
	}

	aname := strings.TrimSpace(st.Anime)
	if aname == "" {
		discordApplyActivity(cfg.DiscordAppID, mgr, nil)
		traySetIdle()
		return
	}

	act := buildActivity(aname, st.episodeLabel(), "", st.CoverURL, cfg.SmallImage, anilistURL(st.AniID), pos, st.Dur, st.Paused, st.Rewatching)
	if !st.Ready {
		delete(act, "timestamps")
	}
	discordApplyActivity(cfg.DiscordAppID, mgr, act)

	percent := -1
	if st.Dur > 0 {
		p := int(pos/st.Dur*100 + 0.5)
		if p < 0 {
			p = 0
		} else if p > 100 {
			p = 100
		}
		percent = p
	}
	traySetWatching(aname, st.Episode, percent, st.TotalEps)
}

func simulStartJoinUI(ctx context.Context, ua string, onJoin func(addr, code string)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	baseURL := "http://" + addr + "/"

	mux := http.NewServeMux()
	srv := &http.Server{Handler: withDarkPage(mux)}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Simulwatching</title>
<style>
body{font-family:system-ui;max-width:720px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px;margin:6px 0}
button{padding:10px 12px;font-size:16px}
small{color:#666}
</style></head>
<body>
<h2>Entrar no Simulwatching</h2>
<p>Digite o <b>IP público:porta</b> do seu amigo e o <b>código</b> exibido no Kouki.</p>
<label>Anfitrião (IP:PORTA)</label>
<input id="host" placeholder="123.45.67.89:45130" />
<label>Código</label>
<input id="code" placeholder="3–18 letras/números" maxlength="18" />
<div style="margin-top:10px"><button onclick="join()">Entrar</button></div>
<p id="status"><small></small></p>
<script>
const statusEl = document.querySelector('#status small');
async function join(){
  const host = document.getElementById('host').value.trim();
  const code = document.getElementById('code').value.trim();
  statusEl.textContent = 'Conectando…';
  const res = await fetch('/api/join', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({host, code})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Falha'; return; }
  statusEl.textContent = 'Conectado. Você pode fechar esta aba.';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Kouki · Concluído</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Conectado</h2>
<p>Tentando fechar esta aba…</p>
<script>setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 100);</script>
<p><small>Se ela não fechar automaticamente, você pode fechá-la agora.</small></p>
</body></html>`)
	})

	selected := make(chan struct{}, 1)
	mux.HandleFunc("/api/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "método não permitido"})
			return
		}
		var body struct {
			Host string `json:"host"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid body"})
			return
		}
		body.Host = strings.TrimSpace(body.Host)
		body.Code = strings.TrimSpace(body.Code)
		if body.Host == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "missing host"})
			return
		}
		if !validateJoinCode(body.Code) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid code"})
			return
		}
		if onJoin != nil {
			onJoin(body.Host, body.Code)
		}
		select {
		case selected <- struct{}{}:
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() { _ = srv.Serve(ln) }()
	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
		case <-time.After(5 * time.Minute):
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c)
		cancel()
	}()

	openBrowser(baseURL)
	return nil
}

func simulStartHostUI(ctx context.Context, onHost func(code string)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	baseURL := "http://" + addr + "/"

	mux := http.NewServeMux()
	srv := &http.Server{Handler: withDarkPage(mux)}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Simulwatching</title>
<style>
body{font-family:system-ui;max-width:720px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px;margin:6px 0}
button{padding:10px 12px;font-size:16px}
small{color:#666}
</style></head>
<body>
<h2>Hospedar Simulwatching</h2>
<p>Escolha um <b>código</b> para esta sessão. Você precisará digitá-lo sempre que hospedar (o Kouki não o salva).</p>
<label>Código</label>
<input id="code" placeholder="3–18 letras/números" maxlength="18" />
<div style="margin-top:10px"><button onclick="startHosting()">Iniciar hospedagem</button></div>
<p id="status"><small></small></p>
<script>
const statusEl = document.querySelector('#status small');
async function startHosting(){
  const code = document.getElementById('code').value.trim();
  statusEl.textContent = 'Iniciando…';
  const res = await fetch('/api/host', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({code})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Falha'; return; }
  statusEl.textContent = 'Hospedagem iniciada. Você pode fechar esta aba.';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Kouki · Concluído</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Hospedando</h2>
<p>Tentando fechar esta aba…</p>
<script>setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 100);</script>
<p><small>Se ela não fechar automaticamente, você pode fechá-la agora.</small></p>
</body></html>`)
	})

	selected := make(chan struct{}, 1)
	mux.HandleFunc("/api/host", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "método não permitido"})
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid body"})
			return
		}
		body.Code = strings.TrimSpace(body.Code)
		if !validateJoinCode(body.Code) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid code"})
			return
		}
		if onHost != nil {
			onHost(body.Code)
		}
		select {
		case selected <- struct{}{}:
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() { _ = srv.Serve(ln) }()
	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
		case <-time.After(5 * time.Minute):
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c)
		cancel()
	}()

	openBrowser(baseURL)
	return nil
}

func simulStartJoin(ctx context.Context, host string, code string) {
	host = strings.TrimSpace(host)
	code = strings.TrimSpace(code)
	if host == "" || !validateJoinCode(code) {
		messageBox("Kouki: Simulwatching", "Anfitrião ou código inválido.", mbOK|mbIconError)
		return
	}
	simulStop()
	simulLoadSettingsFromStore()

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		messageBox("Kouki: Simulwatching", "Não foi possível conectar:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeSimulMsg(conn, "hello", simulHello{Code: code}); err != nil {
		_ = conn.Close()
		messageBox("Kouki: Simulwatching", "Falha na negociação da conexão:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	dec := json.NewDecoder(conn)
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	msg, err := readSimulMsg(dec)
	if err != nil {
		_ = conn.Close()
		messageBox("Kouki: Simulwatching", "Falha na negociação da conexão:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}
	if msg.Type == "error" {
		_ = conn.Close()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(msg.Data, &e)
		messageBox("Kouki: Simulwatching", "Entrada recusada:\n\n"+strings.TrimSpace(e.Error), mbOK|mbIconError)
		return
	}
	if msg.Type != "hello_ok" {
		_ = conn.Close()
		messageBox("Kouki: Simulwatching", "Resposta inesperada do anfitrião.", mbOK|mbIconError)
		return
	}

	simul.mu.Lock()
	simul.mode = simulModeJoin
	simul.conn = conn
	simulSetParticipantsLocked(1)
	simulSetStatusLocked("Conectado")
	simul.mu.Unlock()
	simulRefreshTray()

	clearTrackingState()

	go func() {
		defer func() {
			_ = conn.Close()
			simul.mu.Lock()
			if simul.conn == conn {
				simul.conn = nil
				simul.mode = simulModeOff
				simulSetParticipantsLocked(0)
				simulSetStatusLocked("Desligado")
			}
			simul.mu.Unlock()
			simulRefreshTray()
		}()

		go func() {
			t := time.NewTicker(25 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if err := writeSimulMsg(conn, "ping", nil); err != nil {
						return
					}
				}
			}
		}()

		synced := make(map[string]bool)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
			m, err := readSimulMsg(dec)
			if err != nil {
				return
			}
			switch m.Type {
			case "state":
				var st simulState
				if json.Unmarshal(m.Data, &st) == nil {
					st.UpdatedAt = time.Now().UnixNano()
					simul.mu.Lock()
					simul.lastState = st
					simul.mu.Unlock()
				}
			case "sync":
				var ev simulSyncEvent
				if json.Unmarshal(m.Data, &ev) == nil {
					k := fmt.Sprintf("%d:%d", ev.AniID, ev.Episode)
					if synced[k] {
						continue
					}
					synced[k] = true
					store.mu.RLock()
					joinSync := store.SimulJoinSync
					tok := store.AccessToken
					uid := store.UserID
					store.mu.RUnlock()
					if joinSync && strings.TrimSpace(tok) != "" && uid > 0 && ev.AniID > 0 && ev.Episode > 0 {
						c2, cancel := context.WithTimeout(context.Background(), 8*time.Second)
						_ = syncAniListProgress(c2, tok, uid, ev.AniID, ev.Episode, ev.TotalEps)
						cancel()
					}
				}
			case "participants":
				var p simulParticipants
				if json.Unmarshal(m.Data, &p) == nil {
					simul.mu.Lock()
					simulSetParticipantsLocked(p.Participants)
					simul.mu.Unlock()
					simulRefreshTray()
				}
			case "pong":
			default:
			}
		}
	}()
}

func discordRPCEnabled() bool {
	store.mu.RLock()
	on := store.DiscordRPC
	store.mu.RUnlock()
	return on
}

func discordApplyActivity(appID string, mgr *discordManager, activity map[string]any) {
	if mgr == nil {
		return
	}
	if !discordRPCEnabled() {
		mgr.close(appID)
		return
	}
	mgr.setActivity(appID, activity)
}

func randJoinCode() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%06d", mrand.Intn(1000000))
	}
	const hextbl = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < len(b); i++ {
		out[i*2] = hextbl[b[i]>>4]
		out[i*2+1] = hextbl[b[i]&0x0f]
	}
	return string(out)
}

func writeSimulMsg(w io.Writer, typ string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	b, err := json.Marshal(simulMessage{Type: typ, TS: time.Now().Unix(), Data: raw})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func readSimulMsg(dec *json.Decoder) (simulMessage, error) {
	var m simulMessage
	err := dec.Decode(&m)
	return m, err
}

func detectPublicIP(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=text", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("ipify http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return "", errors.New("invalid public IP")
	}
	return ip, nil
}

func tryPortMapUPnP(ctx context.Context, port int, desc string, lifetimeSeconds uint32) (func(), string, error) {
	if port <= 0 || port > 65535 {
		return nil, "", errors.New("invalid port")
	}
	try := func() (func(), string, error) {
		clients, _, err := internetgateway2.NewWANIPConnection2Clients()
		if err == nil && len(clients) > 0 {
			c := clients[0]
			extIP, _ := c.GetExternalIPAddress()
			_ = c.DeletePortMapping("", uint16(port), "TCP")
			if err := c.AddPortMapping("", uint16(port), "TCP", uint16(port), "", true, desc, lifetimeSeconds); err != nil {
				return nil, "", err
			}
			cleanup := func() { _ = c.DeletePortMapping("", uint16(port), "TCP") }
			return cleanup, extIP, nil
		}
		clients1, _, err1 := internetgateway1.NewWANIPConnection1Clients()
		if err1 == nil && len(clients1) > 0 {
			c := clients1[0]
			extIP, _ := c.GetExternalIPAddress()
			_ = c.DeletePortMapping("", uint16(port), "TCP")
			if err := c.AddPortMapping("", uint16(port), "TCP", uint16(port), "", true, desc, lifetimeSeconds); err != nil {
				return nil, "", err
			}
			cleanup := func() { _ = c.DeletePortMapping("", uint16(port), "TCP") }
			return cleanup, extIP, nil
		}
		return nil, "", errors.New("no UPnP IGD found")
	}

	done := make(chan struct{})
	var cleanup func()
	var extIP string
	var outErr error
	go func() {
		defer close(done)
		cleanup, extIP, outErr = try()
	}()
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-done:
		return cleanup, extIP, outErr
	}
}

func tryPortMapNATPMP(ctx context.Context, port int, lifetimeSeconds int) (func(), string, error) {
	gateways := []string{"192.168.0.1", "192.168.1.1"}
	for _, gw := range gateways {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}

		done := make(chan struct{})
		var (
			cleanup func()
			err     error
		)
		go func(gw string) {
			defer close(done)
			c := natpmp.NewClient(net.ParseIP(gw))
			if c == nil {
				err = errors.New("invalid gateway")
				return
			}
			_, _ = c.GetExternalAddress()
			res, e := c.AddPortMapping("tcp", port, port, lifetimeSeconds)
			if e != nil {
				err = e
				return
			}
			_ = res
			cleanup = func() { _, _ = c.AddPortMapping("tcp", port, port, 0) }
		}(gw)

		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(2 * time.Second):
			continue
		case <-done:
			if err == nil && cleanup != nil {
				return cleanup, "", nil
			}
		}
	}
	return nil, "", errors.New("no NAT-PMP gateway responded")
}

const mpvRPCDeadline = 800 * time.Millisecond

// fallbackLargeImageKey is the Discord asset key used when no AniList cover
// could be resolved. It MUST match an image asset registered on the Discord
// application (Discord Developer Portal -> Rich Presence -> Artwork); changing
// it without uploading a matching asset makes the presence fall back to the
// application's default icon.
const fallbackLargeImageKey = "kouki"

// setTrayIcon gives the notification-area icon the application icon. It reads
// from the embedded FS so a relocated or missing kouki.ico cannot break it.
func setTrayIcon() {
	if b, err := iconAssets.ReadFile(iconFileName); err == nil && len(b) > 0 {
		systray.SetIcon(b)
		return
	}
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), iconFileName)); err == nil {
			systray.SetIcon(b)
		}
	}
}

func logPath() string {
	return filepath.Join(appDataDir(), "kouki.log")
}

// rotateLogIfLarge moves a log that has grown past limit bytes to <name>.old,
// replacing the previous .old. The logs are append-only and were never trimmed,
// so a machine that ran for months kept every line it had ever written.
func rotateLogIfLarge(path string, limit int64) {
	if st, err := os.Stat(path); err == nil && st.Size() > limit {
		_ = os.Remove(path + ".old")
		_ = os.Rename(path, path+".old")
	}
}

func logAppend(lines ...string) {
	path := logPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	rotateLogIfLarge(path, 1<<20)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	// The parts form ONE message. This used to write each part on its own line,
	// which split every value away from its label: the log read
	//
	//	mpv: args=
	//	--idle=yes --force-window=yes ...
	//
	// i.e. the very values needed to diagnose a launch failure were pushed out
	// of sight behind their own empty-looking labels. Callers that want several
	// lines call logAppend once per line.
	_, _ = f.WriteString(time.Now().Format("2006-01-02 15:04:05") +
		" " + strings.Join(lines, "") + "\n")
}

func logRecoveredPanic(where string) {
	if r := recover(); r != nil {
		logAppend("panic in "+where+": ", fmt.Sprint(r))
		logAppend(string(debug.Stack()))
	}
}

// fileWriteMu serialises writes to the settings files, so two goroutines saving
// at once cannot interleave on the same path.
var fileWriteMu sync.Mutex

// writeFileAtomic writes to a sibling temporary file and renames it over the
// target. os.WriteFile truncates the destination first, so a crash or a power
// cut in the middle of a save left auth.json empty — and with it the user's
// AniList and MyAnimeList tokens.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	fileWriteMu.Lock()
	defer fileWriteMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// maxHTTPBody bounds every response body read into memory.
const maxHTTPBody = 8 << 20

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

const windowsRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const windowsRunValueName = appName

// startHiddenArg is passed by the Windows autostart entry. A program that starts
// with the machine belongs in the tray; opening a 1000-pixel window over the
// desktop at every login is the opposite of what "iniciar com o Windows" means.
const startHiddenArg = "--tray"

// startHidden is set from the command line before the window is built.
var startHidden bool

// windowsRunValueExists reports whether an autostart entry is present, whoever
// wrote it (this program, or the installer's "iniciar com o Windows" task).
func windowsRunValueExists() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(windowsRunValueName)
	return err == nil
}

func setWindowsRunOnStartup(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !enabled {
		err := k.DeleteValue(windowsRunValueName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	cmd := "\"" + exe + "\" " + startHiddenArg
	return k.SetStringValue(windowsRunValueName, cmd)
}

func escapePowerShellSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// anilistClientID is the client id actually sent to AniList.
// Override per machine with the ANILIST_CLIENT_ID environment variable.
var anilistClientID = resolveAnilistClientID()

func resolveAnilistClientID() string {
	if v := strings.TrimSpace(os.Getenv("ANILIST_CLIENT_ID")); v != "" {
		return v
	}
	return anilistClientIDDefault
}

const (
	// anilistClientIDDefault is the AniList API client that Kouki authorises
	// against. The name shown on AniList's consent screen comes from THAT
	// registration, not from this code — renaming the app here does nothing.
	//
	// The implicit flow (response_type=token) needs only the client id; no
	// AniList secret is ever used or required by Kouki.
	anilistClientIDDefault = "54089"

	discordAppID         = "1434412611411120198"
	suwayomiDiscordAppID = "1485726894795133021"

	localLoginAddr    = "127.0.0.1:45124"
	localMALLoginAddr = "127.0.0.1:45125"

	anilistGQLURL    = "https://graphql.anilist.co"
	malAPIBaseURL    = "https://api.myanimelist.net/v2"
	malOAuthAuthURL  = "https://myanimelist.net/v1/oauth2/authorize"
	malOAuthTokenURL = "https://myanimelist.net/v1/oauth2/token"
)

var errAniListRateLimited = errors.New("anilist rate limited")

type Config struct {
	DiscordAppID         string
	SuwayomiDiscordAppID string
	MpvPipe              string
	PollInterval         time.Duration
	UserAgent            string
	SmallImage           string

	MALClientID     string
	MALClientSecret string
	MALRedirectURI  string
}

type suwayomiChapterNode struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	ChapterNumber float64 `json:"chapterNumber"`
	LastReadAt    string  `json:"lastReadAt"`
	Manga         struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"manga"`
}

type anilistMangaLite struct {
	ID    int `json:"id"`
	Title struct {
		English string `json:"english"`
		Romaji  string `json:"romaji"`
	} `json:"title"`
	CoverImage struct {
		ExtraLarge string `json:"extraLarge"`
		Large      string `json:"large"`
	} `json:"coverImage"`
	Chapters int    `json:"chapters"`
	Status   string `json:"status"`
}

func suwayomiGraphQLURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "http://localhost:4567"
	}
	base = strings.TrimRight(base, "/")
	return base + "/api/graphql"
}

func suwayomiAlive(ctx context.Context, base string) bool {
	url := suwayomiGraphQLURL(base)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

func fetchSuwayomiRecentChapter(ctx context.Context, base string) (*suwayomiChapterNode, error) {
	const q = `
query RecentChapter {
  chapters(
    orderBy: LAST_READ_AT
    orderByType: DESC
    first: 1
    filter: { lastReadAt: { greaterThan: "0" } }
  ) {
    nodes {
      id
      name
      chapterNumber
      lastReadAt
      manga { id title status }
    }
  }
}`
	url := suwayomiGraphQLURL(base)
	body := map[string]any{"query": q}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("suwayomi http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data struct {
			Chapters struct {
				Nodes []suwayomiChapterNode `json:"nodes"`
			} `json:"chapters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data.Chapters.Nodes) == 0 {
		return nil, nil
	}
	return &out.Data.Chapters.Nodes[0], nil
}

func fetchAniListManga(ctx context.Context, title string) (*anilistMangaLite, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("empty title")
	}
	const q = `
query ($search: String) {
  Media(search: $search, type: MANGA, sort: SEARCH_MATCH) {
    id
    title { english romaji }
    coverImage { extraLarge large }
    chapters
    status
  }
}`
	body := map[string]any{"query": q, "variables": map[string]any{"search": title}}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, errAniListRateLimited
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("anilist http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data struct {
			Media *anilistMangaLite `json:"Media"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data.Media, nil
}

func anilistMangaURL(id int) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("https://anilist.co/manga/%d", id)
}

func bestMangaTitle(al *anilistMangaLite, fallback string) string {
	if al != nil {
		if t := strings.TrimSpace(al.Title.English); t != "" {
			return t
		}
		if t := strings.TrimSpace(al.Title.Romaji); t != "" {
			return t
		}
	}
	return strings.TrimSpace(fallback)
}

func mangaCoverURL(al *anilistMangaLite) string {
	if al == nil {
		return ""
	}
	if s := strings.TrimSpace(al.CoverImage.ExtraLarge); s != "" {
		return s
	}
	if s := strings.TrimSpace(al.CoverImage.Large); s != "" {
		return s
	}
	return ""
}

func suwayomiChapterState(ch *suwayomiChapterNode, al *anilistMangaLite) string {
	if ch == nil {
		return ""
	}
	n := ch.ChapterNumber
	label := "Reading: Ch. "
	if math.Abs(n-math.Round(n)) < 1e-9 {
		label += strconv.Itoa(int(math.Round(n)))
	} else {
		label += strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", n), "0"), ".")
	}

	total := 0
	if al != nil {
		total = al.Chapters
	}
	st := strings.TrimSpace(ch.Manga.Status)
	if strings.EqualFold(st, "COMPLETED") && total > 0 {
		return fmt.Sprintf("%s / %d chapters", label, total)
	}
	return label
}

func buildSuwayomiActivity(title, state, coverURL, smallKey, aniURL string, startedAt time.Time) map[string]any {
	title = strings.TrimSpace(title)
	state = strings.TrimSpace(state)
	if state == "" {
		return nil
	}

	name := "Suwayomi"
	details := title
	if details == "" {
		details = name
	}
	stateLine := state
	img := strings.TrimSpace(coverURL)
	if img == "" {
		img = "suwayomi"
	}
	assets := map[string]any{
		"large_image": img,
		"large_text":  firstNonEmpty(title, details),
	}
	if strings.TrimSpace(aniURL) != "" {
		assets["large_url"] = strings.TrimSpace(aniURL)
	}

	store.mu.RLock()
	showProfile := store.ShowAniProfile && strings.TrimSpace(store.AccessToken) != ""
	username := store.Username
	store.mu.RUnlock()
	if showProfile && strings.TrimSpace(smallKey) != "" {
		assets["small_image"] = strings.TrimSpace(smallKey)
		name := strings.TrimSpace(username)
		if name == "" {
			name = "Usuário do AniList"
		}
		assets["small_text"] = name + " no AniList"
	} else {
		assets["small_image"] = "suwayomi"
		assets["small_text"] = "Suwayomi"
	}

	act := map[string]any{
		"name":    name,
		"details": details,
		"state":   stateLine,
		"type":    3,
		"assets":  assets,
	}
	if !startedAt.IsZero() {
		act["timestamps"] = map[string]any{"start": startedAt.Unix()}
	}
	return act
}

func runSuwayomiRPC(ctx context.Context, cfg Config) {
	defer logRecoveredPanic("runSuwayomiRPC")

	base := strings.TrimSpace(os.Getenv("SUWAYOMI_URL"))
	if base == "" {
		base = "http://localhost:4567"
	}

	poll := 5 * time.Second
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_POLL_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 60 {
			poll = time.Duration(n) * time.Second
		}
	}

	idleTimeout := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_READING_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 && n <= 600 {
			idleTimeout = time.Duration(n) * time.Second
		}
	}

	appID := strings.TrimSpace(cfg.SuwayomiDiscordAppID)
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_DISCORD_APP_ID")); v != "" {
		appID = v
	}
	if appID == "" {
		appID = suwayomiDiscordAppID
	}

	mgr := &discordManager{}
	defer mgr.close(appID)

	cache := make(map[int]*anilistMangaLite)
	var lastMangaID int
	var startedAt time.Time

	lastSeenLastReadAt := ""
	var lastChangedAt time.Time

	t := time.NewTicker(poll)
	defer t.Stop()

	clear := func() {
		mgr.setActivity(appID, nil)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		if !discordRPCEnabled() {
			mgr.close(appID)
			continue
		}

		qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		ch, err := fetchSuwayomiRecentChapter(qctx, base)
		cancel()
		if err != nil {
			q2, c2 := context.WithTimeout(ctx, 2*time.Second)
			alive := suwayomiAlive(q2, base)
			c2()
			if !alive {
				clear()
			}
			continue
		}
		if ch == nil {
			clear()
			continue
		}

		if strings.TrimSpace(ch.LastReadAt) != "" {
			if ch.LastReadAt != lastSeenLastReadAt {
				lastSeenLastReadAt = ch.LastReadAt
				lastChangedAt = time.Now()
			}
		}
		active := !lastChangedAt.IsZero() && time.Since(lastChangedAt) < idleTimeout
		if !active {
			clear()
			continue
		}

		mangaID := ch.Manga.ID
		rawTitle := strings.TrimSpace(ch.Manga.Title)
		al := cache[mangaID]
		if al == nil && rawTitle != "" {
			c3, cancel3 := context.WithTimeout(ctx, 8*time.Second)
			m, err := fetchAniListManga(c3, rawTitle)
			cancel3()
			if err == nil {
				al = m
				cache[mangaID] = m
			}
		}

		title := bestMangaTitle(al, rawTitle)
		state := suwayomiChapterState(ch, al)
		cover := mangaCoverURL(al)
		aniURL := ""
		if al != nil {
			aniURL = anilistMangaURL(al.ID)
		}
		if mangaID != 0 && mangaID != lastMangaID {
			lastMangaID = mangaID
			startedAt = time.Now()
		}
		act := buildSuwayomiActivity(title, state, cover, cfg.SmallImage, aniURL, startedAt)
		mgr.setActivity(appID, act)
	}
}

func defaultSimulPort() int { return 45130 }

func validateSimulPort(p int) bool {
	return p >= 1024 && p <= 65535
}

func validateJoinCode(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) < 3 || len(code) > 18 {
		return false
	}
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func (s simulState) episodeLabel() string {
	ep := strings.TrimSpace(s.Episode)
	if ep == "" {
		return ""
	}
	if s.TotalEps > 0 {
		return fmt.Sprintf("%s/%d", ep, s.TotalEps)
	}
	return fmt.Sprintf("%s/??", ep)
}

func simulModeTitle(m simulMode) string {
	switch m {
	case simulModeHost:
		return "Anfitrião"
	case simulModeJoin:
		return "Conectado"
	default:
		return "Desligado"
	}
}

type resolveRequest struct {
	SeriesKey string
	MediaID   int
}

type resolveMALRequest struct {
	SeriesKey string
	AnimeID   int
}

var resolveCh = make(chan resolveRequest, 8)
var resolveMALCh = make(chan resolveMALRequest, 8)

func loadConfig() Config {
	appID := discordAppID
	suwayomiAppID := suwayomiDiscordAppID
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_DISCORD_APP_ID")); v != "" {
		suwayomiAppID = v
	}
	pipe := mpvPipeName()
	poll := 250 * time.Millisecond
	if v := os.Getenv("POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 200 {
			poll = time.Duration(ms) * time.Millisecond
		}
	}
	ua := os.Getenv("HTTP_USER_AGENT")
	if ua == "" {
		ua = "kouki/1.0 (+https://anilist.co)"
	}
	small := "anilist"

	// MyAnimeList uses an authorization-code flow, which needs BOTH a client id
	// and a client secret. The secret is not distributable: it belongs to
	// whoever registered the MAL application, so Kouki cannot authenticate
	// without the user supplying their own. Resolution order:
	//
	//	build flag   -X main.buildMALClientID / -X main.buildMALClientSecret
	//	environment  MAL_CLIENT_ID / MAL_CLIENT_SECRET
	//	fallback     the upstream id (no secret exists, so login stays disabled)
	malClientID := strings.TrimSpace(buildMALClientID)
	if malClientID == "" {
		malClientID = strings.TrimSpace(os.Getenv("MAL_CLIENT_ID"))
	}
	if malClientID == "" {
		malClientID = "2353edcd897fd4cfc69fdf935b6e11c4"
	}

	malClientSecret := strings.TrimSpace(buildMALClientSecret)
	if malClientSecret == "" {
		malClientSecret = strings.TrimSpace(os.Getenv("MAL_CLIENT_SECRET"))
	}
	if malClientSecret == "" {
		malClientSecret = "f7a80309a3459774b9396f2822a4e8829f0c6d0f93590c5cb5c40ad314868fda"
	}

	malRedirect := "http://" + localMALLoginAddr + "/callback"

	return Config{
		DiscordAppID:         appID,
		SuwayomiDiscordAppID: suwayomiAppID,
		MpvPipe:              pipe,
		PollInterval:         poll,
		UserAgent:            ua,
		SmallImage:           small,

		MALClientID:     malClientID,
		MALClientSecret: malClientSecret,
		MALRedirectURI:  malRedirect,
	}
}

// MyAnimeList OAuth credentials, injectable at link time with:
//
//	go build -ldflags "-X main.buildMALClientID=<id> -X main.buildMALClientSecret=<secret>"
//
// Both are intentionally empty by default: the secret cannot be redistributed.
var (
	buildMALClientID     string
	buildMALClientSecret string
)

const (
	// Project home, used for the HelpLink / URLInfoAbout values shown in
	// Windows "Apps &amp; features".
	githubOwner = "flugeru"
	githubRepo  = "Kouki"
)

// mpvState is a snapshot of what mpv is playing right now.
//
// The JSON IPC layer that fills it lives in mpv.go.
type mpvState struct {
	Path       string
	FileName   string
	MediaTitle string
	Duration   float64
	TimePos    float64
	Pause      bool
	EOFReached bool
}

// extractStreamFilename handles direct-streaming links (Torbox, Real-Debrid,
// AllDebrid, and similar debrid/streaming services) where mpv's reported
// "path"/"filename" is just an opaque ID or token, e.g.:
//
//	82b584e3-2fc3-4e3c-9d38-14db419a8203?token=...&filename=Neon.Genesis.Evangelion.S01E01.1080p...mkv
//
// In that case the *real* media filename lives inside a query parameter
// rather than in the path itself. This pulls it out (URL-decoded) so the
// title/episode parser gets something meaningful to work with instead of a
// UUID. If no such query parameter exists, the input is returned unchanged
// (aside from stripping an unparsable query string, so raw tokens never leak
// into the parser).
func extractStreamFilename(nameOrPath string) string {
	s := strings.TrimSpace(nameOrPath)
	if s == "" {
		return s
	}

	qIdx := strings.IndexByte(s, '?')
	if qIdx < 0 {
		return s
	}

	values, err := url.ParseQuery(s[qIdx+1:])
	if err != nil {
		// Malformed query string - strip it rather than feed a raw token
		// (with "?" and friends) into the filename parser.
		return s[:qIdx]
	}

	for _, key := range []string{"filename", "file_name", "name"} {
		v := strings.TrimSpace(values.Get(key))
		if v == "" {
			continue
		}
		// url.ParseQuery already decodes percent-escapes, but some services
		// double-encode, so try one more pass defensively.
		if decoded, derr := url.QueryUnescape(v); derr == nil && decoded != "" {
			return decoded
		}
		return v
	}

	// No usable filename in the query string; strip it so at least no
	// token/signature garbage reaches the parser.
	return s[:qIdx]
}

type mediaLite struct {
	ID    int `json:"id"`
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	CoverImage struct {
		Large string `json:"large"`
	} `json:"coverImage"`
	StartDate struct {
		Year int `json:"year"`
	} `json:"startDate"`
	Episodes int    `json:"episodes"`
	Format   string `json:"format"`
}

type anilistResp struct {
	Data struct {
		Page struct {
			Media []mediaLite `json:"media"`
		} `json:"Page"`
	} `json:"data"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

const (
	aniStatusCurrent   = "CURRENT"
	aniStatusPlanning  = "PLANNING"
	aniStatusCompleted = "COMPLETED"
	aniStatusDropped   = "DROPPED"
	aniStatusPaused    = "PAUSED"
	aniStatusRepeating = "REPEATING"
)

var (
	reParensYear = regexp.MustCompile(`\s*\((19|20)\d{2}\)`)
	reEpTail     = regexp.MustCompile(`\s*[-–—]\s*(?:ep|episode)?\s*\d{1,4}(?:v\d+)?\s*$`)
	reBrackets   = regexp.MustCompile(`\s*[\[\(][^\]\)]*[\]\)]`)
	reMultiSpace = regexp.MustCompile(`\s{2,}`)
	reAnyYear    = regexp.MustCompile(`\b(19|20)\d{2}\b`)

	reSxxEyy      = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]*E(\d{1,3})\b`)
	reSxxEyyLoose = regexp.MustCompile(`(?i)\bS\d{1,2}[ ._-]*E\d{1,3}\b`)

	reSeasonSNum = regexp.MustCompile(`(?i)\bS(\d{1,2})\b`)
	reSeasonWord = regexp.MustCompile(`(?i)\b(?:season|cour)\s*(\d{1,2})\b`)
	reSeasonOrd  = regexp.MustCompile(`(?i)\b(1st|2nd|3rd|[4-9]th)\s+season\b`)

	reTrailingCRC = regexp.MustCompile(`\s*\[[0-9a-fA-F]{6,}\]\s*$`)
)

var zeroBasedRelease = struct {
	mu    sync.Mutex
	bySig map[string]bool
}{bySig: make(map[string]bool)}

func zeroBasedStorePath() string { return filepath.Join(appDataDir(), "zero_based_releases.json") }

func loadZeroBasedSignatures() {
	b, err := os.ReadFile(zeroBasedStorePath())
	if err != nil {
		return
	}
	var tmp struct {
		Sigs []string `json:"sigs"`
	}
	if json.Unmarshal(b, &tmp) != nil {
		return
	}
	zeroBasedRelease.mu.Lock()
	for _, s := range tmp.Sigs {
		s = strings.TrimSpace(strings.ToLower(s))
		if s != "" {
			zeroBasedRelease.bySig[s] = true
		}
	}
	zeroBasedRelease.mu.Unlock()
}

func saveZeroBasedSignaturesLocked() {

	sigs := make([]string, 0, len(zeroBasedRelease.bySig))
	for s := range zeroBasedRelease.bySig {
		sigs = append(sigs, s)
	}
	sort.Strings(sigs)
	b, _ := json.MarshalIndent(struct {
		Sigs []string `json:"sigs"`
	}{Sigs: sigs}, "", "  ")
	_ = os.WriteFile(zeroBasedStorePath(), b, 0600)
}

func releaseSignatureForZeroBased(nameOrPath string) (sig string, hasEpisodeToken bool) {
	base := filepath.Base(strings.TrimSpace(nameOrPath))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}

	base = reTrailingCRC.ReplaceAllString(base, "")
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}

	validDigitsAllowZero := func(digits string, allow4Digits bool) bool {
		digits = strings.TrimSpace(digits)
		if digits == "" {
			return false
		}

		if strings.TrimLeft(digits, "0") == "" {
			return true
		}
		return normalizeEpisodeCandidate(digits, allow4Digits) != ""
	}

	hasEpisodeToken = reSxxEyy.MatchString(base)
	if hasEpisodeToken {
		base = reSxxEyy.ReplaceAllStringFunc(base, func(m string) string {
			sm := reSxxEyy.FindStringSubmatch(m)
			if len(sm) == 3 {
				return "S" + sm[1] + "E__"
			}
			return "S__E__"
		})

		base = reMultiSpace.ReplaceAllString(base, " ")
		base = strings.TrimSpace(base)
		if base == "" {
			return "", false
		}
		return strings.ToLower(base), true
	}

	if idx := reEyyOnly.FindStringSubmatchIndex(base); len(idx) == 4 {
		digits := base[idx[2]:idx[3]]
		if validDigitsAllowZero(digits, false) {
			base = base[:idx[2]] + "__" + base[idx[3]:]
			base = reMultiSpace.ReplaceAllString(base, " ")
			base = strings.TrimSpace(base)
			if base == "" {
				return "", false
			}
			return strings.ToLower(base), true
		}
	}

	if ms := reEGeneric.FindAllStringSubmatchIndex(base, -1); len(ms) > 0 {
		for _, idx := range ms {
			if len(idx) != 4 {
				continue
			}
			digits := base[idx[2]:idx[3]]
			if !validDigitsAllowZero(digits, true) {
				continue
			}
			base2 := base[:idx[2]] + "__" + base[idx[3]:]
			base2 = reMultiSpace.ReplaceAllString(base2, " ")
			base2 = strings.TrimSpace(base2)
			if base2 == "" {
				continue
			}
			return strings.ToLower(base2), true
		}
	}

	if ms := reDashEp.FindAllStringSubmatchIndex(base, -1); len(ms) > 0 {
		for _, idx := range ms {
			if len(idx) != 4 {
				continue
			}
			digits := base[idx[2]:idx[3]]
			if !validDigitsAllowZero(digits, false) {
				continue
			}
			base2 := base[:idx[2]] + "__" + base[idx[3]:]
			base2 = reMultiSpace.ReplaceAllString(base2, " ")
			base2 = strings.TrimSpace(base2)
			if base2 == "" {
				continue
			}
			return strings.ToLower(base2), true
		}
	}

	if idx := reTrailingNum.FindStringSubmatchIndex(base); len(idx) == 4 {
		digits := base[idx[2]:idx[3]]
		if !validDigitsAllowZero(digits, false) {
			return "", false
		}

		base = base[:idx[2]] + "__" + base[idx[3]:]
		base = reMultiSpace.ReplaceAllString(base, " ")
		base = strings.TrimSpace(base)
		if base == "" {
			return "", false
		}
		return strings.ToLower(base), true
	}

	return "", false
}

func markZeroBasedSignature(sig string) {
	if strings.TrimSpace(sig) == "" {
		return
	}
	zeroBasedRelease.mu.Lock()
	if !zeroBasedRelease.bySig[sig] {
		zeroBasedRelease.bySig[sig] = true
		saveZeroBasedSignaturesLocked()
	}
	zeroBasedRelease.mu.Unlock()
}

func isZeroBasedSignature(sig string) bool {
	zeroBasedRelease.mu.Lock()
	on := zeroBasedRelease.bySig[sig]
	zeroBasedRelease.mu.Unlock()
	return on
}

func dirContainsEpisodeZeroForSignature(filePath string, sig string) bool {
	if strings.TrimSpace(filePath) == "" || strings.TrimSpace(sig) == "" {
		return false
	}
	dir := filepath.Dir(filePath)
	if dir == "" || dir == "." {
		return false
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	limit := 600
	if len(ents) < limit {
		limit = len(ents)
	}
	for i := 0; i < limit; i++ {
		e := ents[i]
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lsig, ok := releaseSignatureForZeroBased(name)
		if !ok || lsig != sig {
			continue
		}

		base := filepath.Base(strings.TrimSpace(name))
		if j := strings.LastIndexByte(base, '.'); j >= 0 {
			base = base[:j]
		}
		base = strings.TrimSpace(reTrailingCRC.ReplaceAllString(base, ""))
		if base == "" {
			continue
		}
		if m := reSxxEyy.FindStringSubmatch(base); len(m) == 3 {
			d := strings.TrimSpace(m[2])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
			continue
		}
		if m := reEyyOnly.FindStringSubmatch(base); len(m) == 2 {
			d := strings.TrimSpace(m[1])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
		}
		if ms := reEGeneric.FindAllStringSubmatch(base, -1); len(ms) > 0 {
			for _, m := range ms {
				if len(m) == 2 {
					d := strings.TrimSpace(m[1])
					if strings.TrimLeft(d, "0") == "" {
						return true
					}
				}
			}
		}
		if ms := reDashEp.FindAllStringSubmatch(base, -1); len(ms) > 0 {
			for _, m := range ms {
				if len(m) == 2 {
					d := strings.TrimSpace(m[1])
					if strings.TrimLeft(d, "0") == "" {
						return true
					}
				}
			}
		}
		if m := reTrailingNum.FindStringSubmatch(base); len(m) == 2 {
			d := strings.TrimSpace(m[1])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
		}
	}
	return false
}

func applyZeroBasedEpisodeFix(filePath string, key string, ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ep
	}
	n, err := strconv.Atoi(ep)
	if err != nil {
		return ep
	}
	sig, ok := releaseSignatureForZeroBased(key)
	if !ok || sig == "" {
		sig, ok = releaseSignatureForZeroBased(filePath)
	}
	if n == 0 {
		if ok && sig != "" {
			markZeroBasedSignature(sig)
		} else {
			if fsig, fok := releaseSignatureForZeroBased(filePath); fok && fsig != "" {
				markZeroBasedSignature(fsig)
			}
		}
		return "1"
	}
	if !ok || sig == "" {
		return ep
	}
	if isZeroBasedSignature(sig) || dirContainsEpisodeZeroForSignature(filePath, sig) {
		markZeroBasedSignature(sig)
		return strconv.Itoa(n + 1)
	}
	return ep
}

func parseSeasonFromString(s string) int {
	if s == "" {
		return 0
	}
	if m := reSxxEyy.FindStringSubmatch(s); len(m) == 3 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}

	if m := reSeasonSNum.FindStringSubmatch(s); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if m := reSeasonWord.FindStringSubmatch(s); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if m := reSeasonOrd.FindStringSubmatch(s); len(m) == 2 {
		s := strings.ToLower(m[1])
		switch s {
		case "1st":
			return 1
		case "2nd":
			return 2
		case "3rd":
			return 3
		default:
			digits := s[:len(s)-2]
			if n, err := strconv.Atoi(digits); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func wantSeasonFrom(sources ...string) int {
	for _, s := range sources {
		if n := parseSeasonFromString(s); n > 0 {
			return n
		}
	}
	return 0
}

func parseYearString(s string) int {
	if s == "" {
		return 0
	}
	if m := reAnyYear.FindString(s); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			return y
		}
	}
	return 0
}

func wantYearFrom(md *habari.Metadata, key, mediaTitle string) int {
	if y, err := strconv.Atoi(strings.TrimSpace(md.Year)); err == nil && y > 1900 {
		return y
	}
	if y := parseYearString(key); y > 0 {
		return y
	}
	if y := parseYearString(mediaTitle); y > 0 {
		return y
	}
	return 0
}

func cleanTitleForSearch(s string) string {
	s = strings.TrimSpace(s)

	if ext := strings.ToLower(filepath.Ext(s)); ext != "" {
		switch ext {
		case ".mkv", ".mp4", ".avi", ".mov", ".webm", ".m4v":
			s = strings.TrimSuffix(s, filepath.Ext(s))
		}
	}

	s = reBrackets.ReplaceAllString(s, "")
	s = reParensYear.ReplaceAllString(s, "")
	s = reAnyYear.ReplaceAllString(s, "")
	s = reSxxEyyLoose.ReplaceAllString(s, "")
	s = reEpTail.ReplaceAllString(s, "")

	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")

	s = reSeasonSNum.ReplaceAllString(s, "")
	s = reSeasonWord.ReplaceAllString(s, "")

	s = strings.Trim(s, " -–—")
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func seriesKeyForOverride(fileKey, parsedTitle, mediaTitle string, wantYear, wantSeason int) string {
	base := firstNonEmpty(parsedTitle, mediaTitle, fileKey)
	base = cleanTitleForSearch(base)
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return ""
	}
	key := base
	if wantYear > 0 {
		key += fmt.Sprintf("#y%d", wantYear)
	}
	if wantSeason > 0 {
		key += fmt.Sprintf("#s%d", wantSeason)
	}
	return key
}

func normalizeForTitleMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(cleanTitleForSearch(s)))
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == ' ':
			return r
		default:
			return ' '
		}
	}, s)
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func tokenSetForTitleMatch(s string) map[string]bool {
	s = normalizeForTitleMatch(s)
	out := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		w = strings.TrimSpace(w)
		if len(w) < 2 {
			continue
		}
		out[w] = true
	}
	return out
}

func overlapCount(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

func pickBest(ms []mediaLite, query string, wantYear, wantSeason int) (n string, c string, i int, totalEps int) {
	if len(ms) == 0 {
		return "", "", 0, 0
	}

	qNorm := normalizeForTitleMatch(query)
	qTokens := tokenSetForTitleMatch(query)
	qTokN := len(qTokens)

	type scored struct {
		M         mediaLite
		Name      string
		Score     float64
		Exact     bool
		Overlap   int
		TokCount  int
		StartYear int
	}

	scoreMedia := func(m mediaLite) scored {
		variants := []string{m.Title.English, m.Title.Romaji, m.Title.Native}
		best := scored{M: m, Name: mediaLiteTitle(m), Score: 0, Exact: false, Overlap: 0, TokCount: 0, StartYear: m.StartDate.Year}
		for _, v := range variants {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			vn := normalizeForTitleMatch(v)
			vt := tokenSetForTitleMatch(v)
			o := overlapCount(qTokens, vt)
			cov := 0.0
			if qTokN > 0 {
				cov = float64(o) / float64(qTokN)
			}
			jacc := 0.0
			if u := qTokN + len(vt) - o; u > 0 {
				jacc = float64(o) / float64(u)
			}
			exact := qNorm != "" && vn == qNorm
			prefix := qNorm != "" && (strings.HasPrefix(vn, qNorm) || strings.HasPrefix(qNorm, vn))
			s := cov + 0.25*jacc
			if prefix {
				s += 0.15
			}
			if exact {
				s += 1.0
			}
			if s > best.Score {
				best.Score = s
				best.Exact = exact
				best.Overlap = o
				best.TokCount = len(vt)
				best.Name = strings.TrimSpace(v)
			}
		}
		return best
	}

	var cands []scored
	for _, m := range ms {
		s := scoreMedia(m)
		if qTokN > 0 {
			if qTokN >= 2 {
				if s.Overlap < 2 && !s.Exact {
					continue
				}
			} else {
				if s.Overlap < 1 && !s.Exact {
					continue
				}
			}
		}
		cands = append(cands, s)
	}
	if len(cands) == 0 {
		return "", "", 0, 0
	}

	base := cands

	if wantSeason > 0 {
		tv := make([]scored, 0, len(base))
		for _, s := range base {
			if strings.EqualFold(s.M.Format, "TV") {
				tv = append(tv, s)
			}
		}
		if len(tv) > 0 {
			base = tv
		}
	}

	sort.Slice(base, func(i, j int) bool {
		if base[i].Exact != base[j].Exact {
			return base[i].Exact
		}
		if base[i].Score != base[j].Score {
			return base[i].Score > base[j].Score
		}
		if base[i].TokCount != base[j].TokCount {
			return base[i].TokCount < base[j].TokCount
		}

		yi, yj := base[i].StartYear, base[j].StartYear
		if yi == 0 && yj != 0 {
			return false
		}
		if yi != 0 && yj == 0 {
			return true
		}
		if yi != yj {
			return yi < yj
		}
		return base[i].M.ID < base[j].M.ID
	})

	if wantSeason > 0 {
		if wantSeason <= len(base) {
			m := base[wantSeason-1].M
			n = mediaLiteTitle(m)
			return n, m.CoverImage.Large, m.ID, m.Episodes
		}
	}

	if wantYear > 0 {
		for _, s := range base {
			m := s.M
			if m.StartDate.Year == wantYear {
				n = mediaLiteTitle(m)
				return n, m.CoverImage.Large, m.ID, m.Episodes
			}
		}
	}

	m := base[0].M
	n = mediaLiteTitle(m)
	return n, m.CoverImage.Large, m.ID, m.Episodes
}

func findAniList(ctx context.Context, rawTitle string, wantYear, wantSeason int, ua string) (name, coverURL string, id int, totalEps int, err error) {
	cands := []string{cleanTitleForSearch(rawTitle), rawTitle}
	const q = `
query($search: String) {
  Page(perPage: 10) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      id
      title { romaji english native }
      coverImage { large }
      startDate { year }
      episodes
      format
    }
  }
}`
	for _, title := range cands {
		body := map[string]any{"query": q, "variables": map[string]any{"search": title}}
		bs, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		resp, rerr := httpClient.Do(req)
		if rerr != nil {
			err = rerr
			continue
		}
		func() {
			defer resp.Body.Close()

			if resp.StatusCode == 429 {
				_, _ = io.Copy(io.Discard, resp.Body)
				err = errAniListRateLimited
				return
			}
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
				err = fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b))
				return
			}
			var ar anilistResp
			if derr := json.NewDecoder(resp.Body).Decode(&ar); derr != nil {
				err = derr
				return
			}
			ms := ar.Data.Page.Media
			if len(ms) == 0 {
				err = errors.New("no match on AniList")
				return
			}
			name, coverURL, id, totalEps = pickBest(ms, title, wantYear, wantSeason)
			if id == 0 {
				err = errors.New("no acceptable match on AniList")
				return
			}
			err = nil
		}()
		if errors.Is(err, errAniListRateLimited) {
			return
		}
		if err == nil && id != 0 {
			return
		}
	}
	if err == nil {
		err = errors.New("no match on AniList")
	}
	return
}

func searchAniList(ctx context.Context, query string, ua string) ([]mediaLite, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("consulta vazia")
	}
	const q = `
query($search: String) {
  Page(perPage: 25) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      id
      title { romaji english native }
      coverImage { large }
      startDate { year }
      episodes
      format
    }
  }
}`
	body := map[string]any{"query": q, "variables": map[string]any{"search": query}}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, errAniListRateLimited
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return nil, fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b))
	}
	var ar anilistResp
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, err
	}
	if len(ar.Data.Page.Media) == 0 {
		return nil, errors.New("no match on AniList")
	}
	return ar.Data.Page.Media, nil
}

func getAniListMediaByID(ctx context.Context, id int, ua string) (mediaLite, error) {
	var out mediaLite
	if id <= 0 {
		return out, errors.New("ID inválido")
	}
	const q = `
query($id: Int) {
  Media(id: $id, type: ANIME) {
    id
    title { romaji english native }
    coverImage { large }
    startDate { year }
    episodes
    format
  }
}`
	body := map[string]any{"query": q, "variables": map[string]any{"id": id}}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return out, errAniListRateLimited
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return out, fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b))
	}
	var respObj struct {
		Data struct {
			Media *mediaLite `json:"Media"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respObj); err != nil {
		return out, err
	}
	if respObj.Data.Media == nil {
		return out, errors.New("media not found")
	}
	return *respObj.Data.Media, nil
}

type malAnimeNode struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	MainPicture struct {
		Large string `json:"large"`
	} `json:"main_picture"`
	StartSeason struct {
		Year int `json:"year"`
	} `json:"start_season"`
	NumEpisodes int    `json:"num_episodes"`
	MediaType   string `json:"media_type"`
}

type malSearchResp struct {
	Data []struct {
		Node malAnimeNode `json:"node"`
	} `json:"data"`
}

func malMediaTypeToFormat(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	switch s {
	case "tv":
		return "TV"
	case "movie":
		return "MOVIE"
	case "ova":
		return "OVA"
	case "ona":
		return "ONA"
	case "special":
		return "SPECIAL"
	case "music":
		return "MUSIC"
	default:
		return strings.ToUpper(s)
	}
}

func malToMediaLite(n malAnimeNode) mediaLite {
	var m mediaLite
	m.ID = n.ID
	m.Title.English = strings.TrimSpace(n.Title)
	m.CoverImage.Large = strings.TrimSpace(n.MainPicture.Large)
	m.StartDate.Year = n.StartSeason.Year
	m.Episodes = n.NumEpisodes
	m.Format = malMediaTypeToFormat(n.MediaType)
	return m
}

func searchMAL(ctx context.Context, query string, clientID string, ua string) ([]mediaLite, error) {
	query = strings.TrimSpace(query)
	clientID = strings.TrimSpace(clientID)
	if query == "" {
		return nil, errors.New("consulta vazia")
	}
	if clientID == "" {
		return nil, errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}

	u, _ := url.Parse(malAPIBaseURL + "/anime")
	q := u.Query()
	q.Set("q", query)
	q.Set("limit", "25")
	q.Set("fields", "id,title,main_picture,start_season,num_episodes,media_type")
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("X-MAL-CLIENT-ID", clientID)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("mal http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var out malSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, errors.New("no match on MyAnimeList")
	}

	ms := make([]mediaLite, 0, len(out.Data))
	for _, row := range out.Data {
		if row.Node.ID <= 0 {
			continue
		}
		ms = append(ms, malToMediaLite(row.Node))
	}
	if len(ms) == 0 {
		return nil, errors.New("no match on MyAnimeList")
	}
	return ms, nil
}

func getMALAnimeByID(ctx context.Context, id int, clientID string, ua string) (mediaLite, error) {
	var out mediaLite
	clientID = strings.TrimSpace(clientID)
	if id <= 0 {
		return out, errors.New("ID inválido")
	}
	if clientID == "" {
		return out, errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}

	u, _ := url.Parse(fmt.Sprintf("%s/anime/%d", malAPIBaseURL, id))
	q := u.Query()
	q.Set("fields", "id,title,main_picture,start_season,num_episodes,media_type")
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("X-MAL-CLIENT-ID", clientID)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("mal http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var node malAnimeNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return out, err
	}
	return malToMediaLite(node), nil
}

func findMAL(ctx context.Context, rawTitle string, wantYear, wantSeason int, clientID string, ua string) (name, coverURL string, id int, totalEps int, err error) {
	cands := []string{cleanTitleForSearch(rawTitle), rawTitle}
	for _, title := range cands {
		ms, rerr := searchMAL(ctx, title, clientID, ua)
		if rerr != nil {
			err = rerr
			continue
		}
		name, coverURL, id, totalEps = pickBest(ms, title, wantYear, wantSeason)
		if id != 0 {
			err = nil
			return
		}
		err = errors.New("no acceptable match on MyAnimeList")
	}
	if err == nil {
		err = errors.New("no match on MyAnimeList")
	}
	return
}

func malAnimeURL(id int) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("https://myanimelist.net/anime/%d", id)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func anilistURL(id int) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("https://anilist.co/anime/%d", id)
}

const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
)

type discordIPC struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

type discordManager struct {
	mu          sync.Mutex
	ipc         *discordIPC
	lastAttempt time.Time
}

func (m *discordManager) ensureConnected(appID string) {
	defer logRecoveredPanic("discordManager.ensureConnected")

	if strings.TrimSpace(appID) == "" || appID == "MISSING_APP_ID" {
		return
	}
	m.mu.Lock()
	if m.ipc != nil {
		m.mu.Unlock()
		return
	}
	if !m.lastAttempt.IsZero() && time.Since(m.lastAttempt) < 2*time.Second {
		m.mu.Unlock()
		return
	}
	m.lastAttempt = time.Now()
	m.mu.Unlock()

	ipc, err := connectDiscordIPC(appID)
	if err != nil {
		return
	}

	m.mu.Lock()
	if m.ipc == nil {
		m.ipc = ipc
		ipc = nil
	}
	m.mu.Unlock()
	if ipc != nil {
		_ = ipc.close()
	}
}

func (m *discordManager) setActivity(appID string, activity map[string]any) {
	defer logRecoveredPanic("discordManager.setActivity")

	if strings.TrimSpace(appID) == "" || appID == "MISSING_APP_ID" {
		return
	}

	m.mu.Lock()
	ipc := m.ipc
	m.mu.Unlock()
	if ipc != nil {
		if err := ipc.setActivity(appID, activity); err == nil {
			return
		}
		m.mu.Lock()
		if m.ipc == ipc {
			m.ipc = nil
		}
		m.mu.Unlock()
		_ = ipc.close()
	}

	m.ensureConnected(appID)
	m.mu.Lock()
	ipc = m.ipc
	m.mu.Unlock()
	if ipc != nil {
		if err := ipc.setActivity(appID, activity); err != nil {
			m.mu.Lock()
			if m.ipc == ipc {
				m.ipc = nil
			}
			m.mu.Unlock()
			_ = ipc.close()
		}
	}
}

func (m *discordManager) close(appID string) {
	defer logRecoveredPanic("discordManager.close")

	m.mu.Lock()
	ipc := m.ipc
	m.ipc = nil
	m.mu.Unlock()
	if ipc != nil {
		_ = ipc.setActivity(appID, nil)
		_ = ipc.close()
	}
}

func connectDiscordIPCWithTimeout(appID string, dialTimeout time.Duration) (*discordIPC, error) {
	if dialTimeout <= 0 {
		dialTimeout = 500 * time.Millisecond
	}
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf(`\\.\pipe\discord-ipc-%d`, i)
		conn, err := winio.DialPipe(path, &dialTimeout)
		if err != nil {
			continue
		}
		ipc := &discordIPC{conn: conn}
		hello := map[string]any{"v": 1, "client_id": appID}
		if err := ipc.write(opHandshake, hello); err != nil {
			_ = conn.Close()
			continue
		}
		return ipc, nil
	}
	return nil, errors.New("could not connect to any discord-ipc pipe")
}

func connectDiscordIPC(appID string) (*discordIPC, error) {
	return connectDiscordIPCWithTimeout(appID, 350*time.Millisecond)
}

func (d *discordIPC) write(code uint32, payload any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], code)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(data)))
	if _, err := d.conn.Write(hdr); err != nil {
		return err
	}
	_, err = d.conn.Write(data)
	return err
}

func (d *discordIPC) close() error {
	_ = d.write(opClose, map[string]any{})
	return d.conn.Close()
}

func (d *discordIPC) setActivity(appID string, activity map[string]any) error {
	args := map[string]any{"pid": os.Getpid(), "activity": activity}
	envelope := map[string]any{
		"cmd": "SET_ACTIVITY", "args": args,
		"nonce": fmt.Sprintf("%d-%d", time.Now().UnixNano(), mrand.Int63()),
	}
	return d.write(opFrame, envelope)
}

type progSmooth struct {
	mu          sync.Mutex
	lastQueryAt time.Time
	lastPos     float64
	duration    float64
	paused      bool
	initialized bool
}

func (p *progSmooth) updateFromMPV(pos, dur float64, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastQueryAt = time.Now()
	p.lastPos = pos
	if dur > 0 {
		p.duration = dur
	}
	p.paused = paused
	p.initialized = true
}

func (p *progSmooth) estimate() (pos, dur float64, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos, dur, paused = p.lastPos, p.duration, p.paused
	if !p.initialized || p.lastQueryAt.IsZero() {
		return
	}
	if !paused && dur > 0 {
		elapsed := time.Since(p.lastQueryAt).Seconds()
		pos = p.lastPos + elapsed
		if pos > dur {
			pos = dur
		}
	}
	return
}

func (p *progSmooth) reset() {
	p.mu.Lock()
	p.lastQueryAt = time.Time{}
	p.lastPos = 0
	p.duration = 0
	p.paused = false
	p.initialized = false
	p.mu.Unlock()
}

func tsRange(now time.Time, cur, dur float64, paused bool) map[string]any {
	if paused || dur <= 0 || cur < 0 || cur > dur {
		return nil
	}
	start := now.Add(-time.Duration(cur) * time.Second).Unix()
	end := now.Add(time.Duration(dur-cur) * time.Second).Unix()
	return map[string]any{"start": start, "end": end}
}

func buildActivity(title, episode, _clock, coverURL, _smallKey, _aniURL string, cur, dur float64, paused bool, rewatching bool) map[string]any {
	details := title
	epText := "Episódio:"
	if strings.TrimSpace(episode) != "" {
		epText = "Episódio " + episode
	}
	parts := make([]string, 0, 3)
	if paused {
		parts = append(parts, "Pausado")
	}
	if rewatching {
		parts = append(parts, "Assistindo novamente")
	}
	parts = append(parts, epText)
	state := strings.Join(parts, " - ")

	img := coverURL
	if strings.TrimSpace(img) == "" {
		img = fallbackLargeImageKey
	}

	assets := map[string]any{
		"large_image": img,
		"large_text":  title,
	}
	if _aniURL != "" {
		assets["large_url"] = _aniURL
	}

	store.mu.RLock()
	showProfile := store.ShowAniProfile && strings.TrimSpace(store.AccessToken) != ""
	username := store.Username
	store.mu.RUnlock()

	if _smallKey != "" && showProfile {
		assets["small_image"] = _smallKey
		name := strings.TrimSpace(username)
		if name == "" {
			name = "Usuário do AniList"
		}
		assets["small_text"] = name + " no AniList"
	}

	act := map[string]any{
		"name":    details,
		"details": details,
		"state":   state,
		"type":    3,
		"assets":  assets,
	}
	if dur > 0 {
		if tr := tsRange(time.Now(), cur, dur, paused); tr != nil {
			act["timestamps"] = tr
		}
	}
	return act
}

type anilistRewatchCacheEntry struct {
	Rewatching bool
	CheckedAt  time.Time
}

var anilistRewatchCache = struct {
	mu    sync.Mutex
	byAni map[int]anilistRewatchCacheEntry
}{byAni: make(map[int]anilistRewatchCacheEntry)}

func anilistIsRewatching(ctx context.Context, aniID int) bool {
	if aniID <= 0 {
		return false
	}
	store.mu.RLock()
	tok := strings.TrimSpace(store.AccessToken)
	uid := store.UserID
	store.mu.RUnlock()
	if tok == "" || uid <= 0 {
		return false
	}

	anilistRewatchCache.mu.Lock()
	if e, ok := anilistRewatchCache.byAni[aniID]; ok {
		if !e.CheckedAt.IsZero() && time.Since(e.CheckedAt) < 15*time.Minute {
			r := e.Rewatching
			anilistRewatchCache.mu.Unlock()
			return r
		}
	}
	anilistRewatchCache.mu.Unlock()

	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	entry, err := getMediaListEntry(c, tok, uid, aniID)
	cancel()
	if err != nil || entry == nil {
		anilistRewatchCache.mu.Lock()
		anilistRewatchCache.byAni[aniID] = anilistRewatchCacheEntry{Rewatching: false, CheckedAt: time.Now()}
		anilistRewatchCache.mu.Unlock()
		return false
	}

	st := strings.ToUpper(strings.TrimSpace(entry.Status))
	rewatch := st == aniStatusCompleted || st == aniStatusRepeating

	anilistRewatchCache.mu.Lock()
	anilistRewatchCache.byAni[aniID] = anilistRewatchCacheEntry{Rewatching: rewatch, CheckedAt: time.Now()}
	anilistRewatchCache.mu.Unlock()
	return rewatch
}

type presenceCache struct {
	mu           sync.Mutex
	lastFile     string
	lastAni      string
	lastEp       string
	lastCover    string
	lastAniID    int
	lastTotalEps int
	lastRewatch  bool
	startEpoch   time.Time
}

func (c *presenceCache) clear() {
	c.mu.Lock()
	c.lastFile = ""
	c.lastAni = ""
	c.lastEp = ""
	c.lastCover = ""
	c.lastAniID = 0
	c.lastTotalEps = 0
	c.lastRewatch = false
	c.startEpoch = time.Time{}
	c.mu.Unlock()
}

var (
	reEGeneric    = regexp.MustCompile(`(?i)\b(?:ep|eps|episode)\s*[-_. ]*\s*(\d{1,4})\b`)
	reDashEp      = regexp.MustCompile(`(?i)(?:^|\s)[-–—]\s*(\d{1,3})(?:v\d+)?\b`)
	reEyyOnly     = regexp.MustCompile(`(?i)\bE(\d{1,3})\b`)
	reTrailingNum = regexp.MustCompile(`(?:^|[\s._-])(\d{1,3})(?:v\d+)?\s*$`)
)

func pickEpisode(md *habari.Metadata, fallbackTitle string) (titleOut string, ep string) {
	titleOut = firstNonEmpty(md.FormattedTitle, md.Title, fallbackTitle)
	for _, arr := range [][]string{md.EpisodeNumber, md.EpisodeNumberAlt, md.OtherEpisodeNumber} {
		if len(arr) > 0 && strings.TrimSpace(arr[0]) != "" {
			raw := strings.TrimSpace(arr[0])
			if i := strings.IndexByte(raw, 'v'); i > 0 {
				raw = raw[:i]
			}
			ep = strings.TrimLeft(raw, "0")
			if ep == "" {
				ep = "0"
			}
			return
		}
	}
	if ep == "" {
		ep = guessEpisodeFromString(md.FileName)
	}
	if ep == "" {
		ep = guessEpisodeFromString(fallbackTitle)
	}
	return
}

func normalizeEpisodeCandidate(numStr string, allow4Digits bool) string {
	numStr = strings.TrimSpace(numStr)
	if numStr == "" {
		return ""
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return ""
	}
	if n >= 1900 && n <= 2099 {
		return ""
	}
	switch n {
	case 2160, 1440, 1080, 720, 576, 480, 4320:
		return ""
	case 264, 265:
		return ""
	}
	if !allow4Digits && n >= 1000 {
		return ""
	}
	return strconv.Itoa(n)
}

func guessEpisodeFromString(s string) string {
	if s == "" {
		return ""
	}
	if ms := reEGeneric.FindAllStringSubmatch(s, -1); len(ms) > 0 {
		for _, m := range ms {
			if len(m) == 2 {
				if ep := normalizeEpisodeCandidate(m[1], true); ep != "" {
					return ep
				}
			}
		}
	}
	if m := reSxxEyy.FindStringSubmatch(s); len(m) == 3 {
		epDigits := strings.TrimSpace(m[2])
		if strings.TrimLeft(epDigits, "0") == "" {
			return "0"
		}
		if ep := normalizeEpisodeCandidate(epDigits, true); ep != "" {
			return ep
		}
	}
	if ms := reDashEp.FindAllStringSubmatch(s, -1); len(ms) > 0 {
		for _, m := range ms {
			if len(m) == 2 {
				if ep := normalizeEpisodeCandidate(m[1], false); ep != "" {
					return ep
				}
			}
		}
	}
	if m := reTrailingNum.FindStringSubmatch(s); len(m) == 2 {
		if ep := normalizeEpisodeCandidate(m[1], false); ep != "" {
			return ep
		}
	}
	return ""
}

const trayTitleBase = "Kouki"

var (
	menuQuit         *systray.MenuItem
	menuShowWindow   *systray.MenuItem
	menuAbout        *systray.MenuItem
	menuOpenFolder   *systray.MenuItem
	menuLogin        *systray.MenuItem
	menuLogout       *systray.MenuItem
	menuAniListTrack *systray.MenuItem
	menuFillerWarn   *systray.MenuItem
	menuCorrectAnime *systray.MenuItem

	menuMALLogin        *systray.MenuItem
	menuMALLogout       *systray.MenuItem
	menuMALTrack        *systray.MenuItem
	menuMALCorrectAnime *systray.MenuItem

	menuRunOnStartup  *systray.MenuItem
	menuSimul         *systray.MenuItem
	menuSimulHost     *systray.MenuItem
	menuSimulJoin     *systray.MenuItem
	menuSimulStop     *systray.MenuItem
	menuSimulStatus   *systray.MenuItem
	menuSimulJoinSync *systray.MenuItem

	loginCancelFunc context.CancelFunc
	loginCancelMu   sync.Mutex

	malLoginCancelFunc context.CancelFunc
	malLoginCancelMu   sync.Mutex
)

type trackState struct {
	mu              sync.RWMutex
	Active          bool
	SeriesKey       string
	ParsedTitle     string
	MediaTitle      string
	WantYear        int
	WantSeason      int
	LastFileKey     string
	LastAniID       int
	LastTotalEps    int
	LastMALAnimeID  int
	LastMALTotalEps int
}

var curTrack trackState

func setCorrectionMenuActive(active bool) {
	if menuCorrectAnime != nil {
		if active {
			menuCorrectAnime.Enable()
		} else {
			menuCorrectAnime.Disable()
		}
	}
	if menuMALCorrectAnime != nil {
		if active {
			menuMALCorrectAnime.Enable()
		} else {
			menuMALCorrectAnime.Disable()
		}
	}
}

func clearTrackingState() {
	curTrack.mu.Lock()
	curTrack.Active = false
	curTrack.SeriesKey = ""
	curTrack.ParsedTitle = ""
	curTrack.MediaTitle = ""
	curTrack.WantYear = 0
	curTrack.WantSeason = 0
	curTrack.LastFileKey = ""
	curTrack.LastAniID = 0
	curTrack.LastTotalEps = 0
	curTrack.LastMALAnimeID = 0
	curTrack.LastMALTotalEps = 0
	curTrack.mu.Unlock()
	setCorrectionMenuActive(false)
}

func traySetIdle() {
	systray.SetTooltip(trayTitleBase)
}

func traySetWatching(name, ep string, percent int, totalEps int) {
	name = strings.TrimSpace(name)
	ep = strings.TrimSpace(ep)
	if name == "" {
		systray.SetTooltip(trayTitleBase)
		return
	}

	epPart := ""
	if ep != "" {
		if totalEps > 0 {
			epPart = fmt.Sprintf(" · Ep %s/%d", ep, totalEps)
		} else {
			epPart = fmt.Sprintf(" · Ep %s/??", ep)
		}
	}

	pctPart := ""
	if percent >= 0 {
		pctPart = fmt.Sprintf(" · %d%%", percent)
	}

	tooltip := name + epPart + pctPart
	systray.SetTooltip(tooltip)
}

func traySetResolving(name, ep string, wait time.Duration, attempt int) {
	name = strings.TrimSpace(name)
	ep = strings.TrimSpace(ep)
	secs := int(wait.Seconds())
	base := name
	if base == "" {
		base = trayTitleBase
	}

	epPart := ""
	if ep != "" {
		epPart = " · Ep " + ep + "/??"
	}
	msg := fmt.Sprintf("%s%s · AniList rate limited, retrying in %ds (try %d)", base, epPart, secs, attempt)
	systray.SetTooltip(msg)
}

func refreshAuthMenu() {
	store.mu.RLock()
	hasToken := store.AccessToken != ""
	username := store.Username
	trackOn := store.AniListTracking
	warnFiller := store.WarnFiller

	malHasToken := strings.TrimSpace(store.MALAccessToken) != ""
	malUsername := strings.TrimSpace(store.MALUsername)
	malTrackOn := store.MALTracking
	store.mu.RUnlock()

	loginCancelMu.Lock()
	isLoggingIn := loginCancelFunc != nil
	loginCancelMu.Unlock()

	malLoginCancelMu.Lock()
	isMALLoggingIn := malLoginCancelFunc != nil
	malLoginCancelMu.Unlock()

	if menuLogin != nil && menuLogout != nil {
		if isLoggingIn {
			menuLogin.SetTitle("Cancelar login…")
			menuLogin.Enable()
			menuLogout.Disable()
		} else if hasToken {
			title := "AniList: conectado"
			if username != "" {
				title = "Conectado como @" + username
			}
			menuLogin.SetTitle(title)
			menuLogin.Disable()
			menuLogout.Enable()

			if menuAniListTrack != nil {
				menuAniListTrack.Enable()
				if trackOn {
					menuAniListTrack.Check()
				} else {
					menuAniListTrack.Uncheck()
				}
			}

		} else {
			menuLogin.SetTitle("Entrar no AniList…")
			menuLogin.Enable()
			menuLogout.Disable()

			if menuAniListTrack != nil {
				if trackOn {
					menuAniListTrack.Check()
				} else {
					menuAniListTrack.Uncheck()
				}
				menuAniListTrack.Disable()
			}
		}
	}

	if menuMALLogin != nil && menuMALLogout != nil {
		if isMALLoggingIn {
			menuMALLogin.SetTitle("Cancelar login…")
			menuMALLogin.Enable()
			menuMALLogout.Disable()
		} else if malHasToken {
			title := "MyAnimeList: conectado"
			if malUsername != "" {
				title = "MAL: conectado como " + malUsername
			}
			menuMALLogin.SetTitle(title)
			menuMALLogin.Disable()
			menuMALLogout.Enable()

			if menuMALTrack != nil {
				menuMALTrack.Enable()
				if malTrackOn {
					menuMALTrack.Check()
				} else {
					menuMALTrack.Uncheck()
				}
			}
		} else {
			menuMALLogin.SetTitle("Entrar no MyAnimeList…")
			menuMALLogin.Enable()
			menuMALLogout.Disable()

			if menuMALTrack != nil {
				if malTrackOn {
					menuMALTrack.Check()
				} else {
					menuMALTrack.Uncheck()
				}
				menuMALTrack.Disable()
			}
		}
	}

	if menuFillerWarn != nil {
		if warnFiller {
			menuFillerWarn.Check()
		} else {
			menuFillerWarn.Uncheck()
		}
	}
}

type authStore struct {
	Path            string
	AccessToken     string
	Username        string
	UserID          int
	ShowAniProfile  bool
	AniListTracking bool `json:"anilist_tracking"`

	MALAccessToken    string
	MALRefreshToken   string
	MALTokenExpiresAt int64
	MALUsername       string
	MALUserID         int
	MALTracking       bool `json:"mal_tracking"`

	DiscordRPC    bool `json:"discord_rpc"`
	WarnFiller    bool `json:"warn_filler"`
	RunOnStartup  bool `json:"run_on_startup"`
	SimulPort     int  `json:"simul_port"`
	SimulAutoUPnP bool `json:"simul_auto_upnp"`
	SimulJoinSync bool `json:"simul_join_sync"`
	mu            sync.RWMutex
	saveMu        sync.Mutex
}

// appDataDir is the per-user configuration directory: %APPDATA%\Kouki.
//
// Legacy Kouki settings are migrated on first use (copy, never delete) so an
// upgrade keeps the user's AniList/MAL tokens and preferences.
func appDataDir() string {
	return ConfigDir()
}

func (a *authStore) file() string { return filepath.Join(a.Path, "auth.json") }

func (a *authStore) Load() {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := os.ReadFile(a.file())
	if err != nil {
		return
	}
	var tmp struct {
		AccessToken     string `json:"access_token"`
		Username        string `json:"username"`
		UserID          int    `json:"user_id"`
		ShowAniProfile  bool   `json:"show_ani_profile"`
		AniListTracking *bool  `json:"anilist_tracking"`

		MALAccessToken    string `json:"mal_access_token"`
		MALRefreshToken   string `json:"mal_refresh_token"`
		MALTokenExpiresAt int64  `json:"mal_token_expires_at"`
		MALUsername       string `json:"mal_username"`
		MALUserID         int    `json:"mal_user_id"`
		MALTracking       *bool  `json:"mal_tracking"`

		DiscordRPC    *bool `json:"discord_rpc"`
		WarnFiller    *bool `json:"warn_filler"`
		RunOnStartup  bool  `json:"run_on_startup"`
		SimulPort     int   `json:"simul_port"`
		SimulAutoUPnP *bool `json:"simul_auto_upnp"`
		SimulJoinSync bool  `json:"simul_join_sync"`
	}
	if json.Unmarshal(b, &tmp) == nil {
		a.AccessToken = tmp.AccessToken
		a.Username = tmp.Username
		a.UserID = tmp.UserID
		a.ShowAniProfile = tmp.ShowAniProfile
		if tmp.AniListTracking == nil {
			a.AniListTracking = true
		} else {
			a.AniListTracking = *tmp.AniListTracking
		}

		a.MALAccessToken = tmp.MALAccessToken
		a.MALRefreshToken = tmp.MALRefreshToken
		a.MALTokenExpiresAt = tmp.MALTokenExpiresAt
		a.MALUsername = tmp.MALUsername
		a.MALUserID = tmp.MALUserID
		if tmp.MALTracking == nil {
			a.MALTracking = true
		} else {
			a.MALTracking = *tmp.MALTracking
		}

		if tmp.DiscordRPC == nil {
			a.DiscordRPC = true
		} else {
			a.DiscordRPC = *tmp.DiscordRPC
		}
		if tmp.WarnFiller == nil {
			a.WarnFiller = true
		} else {
			a.WarnFiller = *tmp.WarnFiller
		}
		a.RunOnStartup = tmp.RunOnStartup
		if validateSimulPort(tmp.SimulPort) {
			a.SimulPort = tmp.SimulPort
		} else {
			a.SimulPort = defaultSimulPort()
		}
		if tmp.SimulAutoUPnP == nil {
			a.SimulAutoUPnP = true
		} else {
			a.SimulAutoUPnP = *tmp.SimulAutoUPnP
		}
		a.SimulJoinSync = tmp.SimulJoinSync
	}
}

func (a *authStore) Save() {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.RLock()
	data := struct {
		AccessToken     string `json:"access_token"`
		Username        string `json:"username"`
		UserID          int    `json:"user_id"`
		ShowAniProfile  bool   `json:"show_ani_profile"`
		AniListTracking bool   `json:"anilist_tracking"`

		MALAccessToken    string `json:"mal_access_token"`
		MALRefreshToken   string `json:"mal_refresh_token"`
		MALTokenExpiresAt int64  `json:"mal_token_expires_at"`
		MALUsername       string `json:"mal_username"`
		MALUserID         int    `json:"mal_user_id"`
		MALTracking       bool   `json:"mal_tracking"`

		DiscordRPC    bool `json:"discord_rpc"`
		WarnFiller    bool `json:"warn_filler"`
		RunOnStartup  bool `json:"run_on_startup"`
		SimulPort     int  `json:"simul_port"`
		SimulAutoUPnP bool `json:"simul_auto_upnp"`
		SimulJoinSync bool `json:"simul_join_sync"`
	}{
		a.AccessToken,
		a.Username,
		a.UserID,
		a.ShowAniProfile,
		a.AniListTracking,

		a.MALAccessToken,
		a.MALRefreshToken,
		a.MALTokenExpiresAt,
		a.MALUsername,
		a.MALUserID,
		a.MALTracking,

		a.DiscordRPC,
		a.WarnFiller,
		a.RunOnStartup,
		a.SimulPort,
		a.SimulAutoUPnP,
		a.SimulJoinSync,
	}
	a.mu.RUnlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	if err := writeFileAtomic(a.file(), b, 0600); err != nil {
		logAppend("auth: falha ao salvar: ", err.Error())
	}
}

func (a *authStore) Clear() {
	a.mu.Lock()
	a.AccessToken = ""
	a.Username = ""
	a.UserID = 0
	a.ShowAniProfile = false
	a.AniListTracking = true

	a.MALAccessToken = ""
	a.MALRefreshToken = ""
	a.MALTokenExpiresAt = 0
	a.MALUsername = ""
	a.MALUserID = 0
	a.MALTracking = true

	a.DiscordRPC = true
	a.RunOnStartup = false
	a.SimulPort = 0
	a.SimulAutoUPnP = true
	a.SimulJoinSync = false
	a.mu.Unlock()
	_ = os.Remove(a.file())
}

// ClearAniList signs out of AniList and nothing else. Clear wipes the whole
// store — MyAnimeList tokens, every preference — and deletes auth.json, which is
// far more than "Sair" on the AniList card promises.
func (a *authStore) ClearAniList() {
	a.mu.Lock()
	a.AccessToken = ""
	a.Username = ""
	a.UserID = 0
	a.ShowAniProfile = false
	a.mu.Unlock()
	a.Save()
}

func (a *authStore) ClearMAL() {
	a.mu.Lock()
	a.MALAccessToken = ""
	a.MALRefreshToken = ""
	a.MALTokenExpiresAt = 0
	a.MALUsername = ""
	a.MALUserID = 0
	a.mu.Unlock()
	a.Save()
}

var store = &authStore{Path: appDataDir(), WarnFiller: true, DiscordRPC: true, AniListTracking: true, MALTracking: true}

type overrideStore struct {
	Path     string
	FileName string
	mu       sync.RWMutex
	saveMu   sync.Mutex
	ByK      map[string]int `json:"by_key"`
}

func (o *overrideStore) file() string {
	name := strings.TrimSpace(o.FileName)
	if name == "" {
		name = "overrides.json"
	}
	return filepath.Join(o.Path, name)
}

func (o *overrideStore) Load() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ByK == nil {
		o.ByK = make(map[string]int)
	}
	b, err := os.ReadFile(o.file())
	if err != nil {
		return
	}
	var tmp struct {
		ByK map[string]int `json:"by_key"`
	}
	if json.Unmarshal(b, &tmp) == nil {
		if tmp.ByK != nil {
			o.ByK = tmp.ByK
		}
	}
}

func (o *overrideStore) Save() {
	o.saveMu.Lock()
	defer o.saveMu.Unlock()
	o.mu.RLock()
	copyByKey := make(map[string]int, len(o.ByK))
	for key, mediaID := range o.ByK {
		copyByKey[key] = mediaID
	}
	data := struct {
		ByK map[string]int `json:"by_key"`
	}{ByK: copyByKey}
	o.mu.RUnlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	if err := writeFileAtomic(o.file(), b, 0600); err != nil {
		logAppend("overrides: falha ao salvar: ", err.Error())
	}
}

func (o *overrideStore) Get(key string) int {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.ByK == nil {
		return 0
	}
	return o.ByK[key]
}

func (o *overrideStore) Set(key string, mediaID int) {
	key = strings.TrimSpace(key)
	if key == "" || mediaID <= 0 {
		return
	}
	o.mu.Lock()
	if o.ByK == nil {
		o.ByK = make(map[string]int)
	}
	o.ByK[key] = mediaID
	o.mu.Unlock()
	o.Save()
}

var overrides = &overrideStore{Path: appDataDir()}
var malOverrides = &overrideStore{Path: appDataDir(), FileName: "overrides_mal.json"}

func mediaLiteTitle(m mediaLite) string {
	name := strings.TrimSpace(m.Title.English)
	if name == "" {
		name = firstNonEmpty(m.Title.Romaji, m.Title.Native)
	}
	return strings.TrimSpace(name)
}

func startAniListSelector(initialQuery string, ua string, onSelect func(id int)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	baseURL := "http://" + addr + "/"

	mux := http.NewServeMux()
	srv := &http.Server{Handler: withDarkPage(mux)}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Selecionar anime</title>
<style>
body{font-family:system-ui;max-width:900px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px}
.row{display:flex;gap:12px;align-items:center;padding:10px;border-bottom:1px solid #eee}
button{padding:6px 10px}
small{color:#666}
</style></head>
<body>
<h2>Selecionar anime correto</h2>
<p>Pesquise no AniList e clique em <b>Selecionar</b>.</p>
<input id="q" placeholder="Pesquisar…" />
<div id="status"><small></small></div>
<div id="results"></div>
<script>
const q = document.getElementById('q');
const results = document.getElementById('results');
const statusEl = document.querySelector('#status small');

function esc(s){return (s||'').replace(/[&<>\"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));}

async function search() {
  const term = q.value.trim();
  if (!term) { results.innerHTML=''; statusEl.textContent=''; return; }
  statusEl.textContent = 'Pesquisando…';
  const res = await fetch('/api/search?q=' + encodeURIComponent(term));
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Falha na pesquisa'; return; }
  statusEl.textContent = 'Resultados: ' + data.results.length;
  results.innerHTML = data.results.map(m => {
    const meta = [m.format, m.year ? ('ano ' + m.year) : '', m.episodes ? (m.episodes + ' eps.') : ''].filter(Boolean).join(' · ');
    return '<div class="row">'
      + '<div style="flex:1">'
        + '<div><b>' + esc(m.title) + '</b></div>'
        + '<div><small>' + esc(meta) + ' · id ' + m.id + '</small></div>'
      + '</div>'
      + '<button onclick="selectID(' + m.id + ')">Selecionar</button>'
    + '</div>';
  }).join('');
}

async function selectID(id) {
  statusEl.textContent = 'Salvando…';
  const res = await fetch('/api/select', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Falha'; return; }
  // Try to close the tab. Some browsers will block window.close() for tabs
  // not opened by script; in that case we navigate to /done which tries again.
  statusEl.textContent = 'Salvo. Fechando…';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}

q.addEventListener('input', () => { clearTimeout(window._t); window._t = setTimeout(search, 250); });

const params = new URLSearchParams(window.location.search);
const init = params.get('q');
if (init) { q.value = init; search(); }
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Concluído</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Salvo</h2>
<p>Tentando fechar esta aba…</p>
<script>
setTimeout(() => {
  try { window.open('', '_self'); window.close(); } catch (e) {}
}, 100);
</script>
<p><small>Se ela não fechar automaticamente, você pode fechá-la agora.</small></p>
</body></html>`)
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "consulta vazia"})
			return
		}
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ms, err := searchAniList(c, q, ua)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		type row struct {
			ID       int    `json:"id"`
			Title    string `json:"title"`
			Year     int    `json:"year"`
			Format   string `json:"format"`
			Episodes int    `json:"episodes"`
		}
		rows := make([]row, 0, len(ms))
		for _, m := range ms {
			rows = append(rows, row{ID: m.ID, Title: mediaLiteTitle(m), Year: m.StartDate.Year, Format: m.Format, Episodes: m.Episodes})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": rows})
	})

	selected := make(chan int, 1)
	mux.HandleFunc("/api/select", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "método não permitido"})
			return
		}
		var body struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "ID inválido"})
			return
		}
		select {
		case selected <- body.ID:
		default:
		}
		if onSelect != nil {
			onSelect(body.ID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() {
		_ = srv.Serve(ln)
	}()

	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(c)
			cancel()
		case <-time.After(5 * time.Minute):
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(c)
			cancel()
		}
	}()

	url := baseURL
	if strings.TrimSpace(initialQuery) != "" {
		url += "?q=" + urlQueryEscape(initialQuery)
	}
	openBrowser(url)
	return nil
}

func startMALSelector(initialQuery string, clientID string, ua string, onSelect func(id int)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	baseURL := "http://" + addr + "/"

	mux := http.NewServeMux()
	srv := &http.Server{Handler: withDarkPage(mux)}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Selecionar anime</title>
<style>
body{font-family:system-ui;max-width:900px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px}
.row{display:flex;gap:12px;align-items:center;padding:10px;border-bottom:1px solid #eee}
button{padding:6px 10px}
small{color:#666}
</style></head>
<body>
<h2>Selecionar anime correto</h2>
<p>Pesquise no MyAnimeList e clique em <b>Selecionar</b>.</p>
<input id="q" placeholder="Pesquisar…" />
<div id="status"><small></small></div>
<div id="results"></div>
<script>
const q = document.getElementById('q');
const results = document.getElementById('results');
const statusEl = document.querySelector('#status small');

function esc(s){return (s||'').replace(/[&<>\"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));}

async function search() {
  const term = q.value.trim();
  if (!term) { results.innerHTML=''; statusEl.textContent=''; return; }
  statusEl.textContent = 'Pesquisando…';
  const res = await fetch('/api/search?q=' + encodeURIComponent(term));
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Falha na pesquisa'; return; }
  statusEl.textContent = 'Resultados: ' + data.results.length;
  results.innerHTML = data.results.map(m => {
    const meta = [m.format, m.year ? ('ano ' + m.year) : '', m.episodes ? (m.episodes + ' eps.') : ''].filter(Boolean).join(' · ');
    return '<div class="row">'
      + '<div style="flex:1">'
        + '<div><b>' + esc(m.title) + '</b></div>'
        + '<div><small>' + esc(meta) + ' · id ' + m.id + '</small></div>'
      + '</div>'
      + '<button onclick="selectID(' + m.id + ')">Selecionar</button>'
    + '</div>';
  }).join('');
}

async function selectID(id) {
  statusEl.textContent = 'Salvando…';
  const res = await fetch('/api/select', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Falha'; return; }
  statusEl.textContent = 'Salvo. Fechando…';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}

q.addEventListener('input', () => { clearTimeout(window._t); window._t = setTimeout(search, 250); });

const params = new URLSearchParams(window.location.search);
const init = params.get('q');
if (init) { q.value = init; search(); }
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Concluído</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Salvo</h2>
<p>Tentando fechar esta aba…</p>
<script>
setTimeout(() => {
  try { window.open('', '_self'); window.close(); } catch (e) {}
}, 100);
</script>
<p><small>Se ela não fechar automaticamente, você pode fechá-la agora.</small></p>
</body></html>`)
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "consulta vazia"})
			return
		}
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ms, err := searchMAL(c, q, clientID, ua)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		type row struct {
			ID       int    `json:"id"`
			Title    string `json:"title"`
			Year     int    `json:"year"`
			Format   string `json:"format"`
			Episodes int    `json:"episodes"`
		}
		rows := make([]row, 0, len(ms))
		for _, m := range ms {
			rows = append(rows, row{ID: m.ID, Title: mediaLiteTitle(m), Year: m.StartDate.Year, Format: m.Format, Episodes: m.Episodes})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": rows})
	})

	selected := make(chan int, 1)
	mux.HandleFunc("/api/select", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "método não permitido"})
			return
		}
		var body struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "ID inválido"})
			return
		}
		select {
		case selected <- body.ID:
		default:
		}
		if onSelect != nil {
			onSelect(body.ID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() {
		_ = srv.Serve(ln)
	}()

	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(c)
			cancel()
		case <-time.After(5 * time.Minute):
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(c)
			cancel()
		}
	}()

	url := baseURL
	if strings.TrimSpace(initialQuery) != "" {
		url += "?q=" + urlQueryEscape(initialQuery)
	}
	openBrowser(url)
	return nil
}

func urlQueryEscape(s string) string {
	repl := strings.NewReplacer(
		"%", "%25",
		" ", "%20",
		"+", "%2B",
		"&", "%26",
		"?", "%3F",
		"#", "%23",
		"=", "%3D",
	)
	return repl.Replace(s)
}

func openBrowser(u string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

func oauthLogin(ctx context.Context) error {
	clientID := anilistClientID

	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Addr: localLoginAddr}
	mux := http.NewServeMux()

	receivedResponse := false
	var responseMu sync.Mutex

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		responseMu.Lock()
		receivedResponse = true
		responseMu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Login do AniList</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Concluindo login…</h2>
<p id="status">Processando autenticação…</p>
<script>
(function() {
	// The access token is in the URL fragment (after #)
	const hash = window.location.hash.substring(1); // Remove the leading #
	
	if (!hash) {
		document.getElementById('status').textContent = 'Nenhum dado de autenticação encontrado. Tente novamente.';
		return;
	}
	
	const params = new URLSearchParams(hash);
	const token = params.get('access_token');
	const error = params.get('error');
	
	if (error) {
		document.getElementById('status').textContent = 'Falha na autenticação: ' + error;
		fetch('/submit', {
			method: 'POST',
			headers: {'Content-Type': 'application/json'},
			body: JSON.stringify({error: error})
		});
		return;
	}
	
	if (token) {
		document.getElementById('status').textContent = 'Login realizado com sucesso! Fechando…';
		fetch('/submit', {
			method: 'POST',
			headers: {'Content-Type': 'application/json'},
			body: JSON.stringify({token: token})
		}).then(response => {
			if (response.ok) {
				document.getElementById('status').textContent = 'Login realizado com sucesso! Você pode fechar esta janela.';
				setTimeout(() => window.close(), 1500);
			} else {
				document.getElementById('status').textContent = 'Falha no login. Tente novamente.';
			}
		}).catch(err => {
			document.getElementById('status').textContent = 'Erro: ' + err.message;
		});
	} else {
		document.getElementById('status').textContent = 'Nenhum token de acesso encontrado. Tente novamente.';
	}
})();
</script>
</body></html>`)
	})

	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "método não permitido", http.StatusMethodNotAllowed)
			return
		}
		// Only the callback page itself posts here, and it sends JSON. Any other
		// website could otherwise push a token of its own into this endpoint with
		// a "simple" cross-origin request (text/plain needs no preflight), signing
		// the user in to somebody else's account. application/json does require a
		// preflight, and this server never answers one.
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			http.Error(w, "tipo de conteúdo inválido", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)

		var body struct {
			Token string `json:"token"`
			Error string `json:"error"`
		}

		// Non-blocking sends: a repeated POST after the first result would
		// otherwise park this handler forever on a full channel.
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			select {
			case errCh <- fmt.Errorf("resposta inválida do navegador: %w", err):
			default:
			}
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		if body.Error != "" {
			select {
			case errCh <- fmt.Errorf("o AniList recusou o login: %s", body.Error):
			default:
			}
			http.Error(w, "authentication error", http.StatusBadRequest)
			return
		}

		tok := strings.TrimSpace(body.Token)
		if tok == "" {
			select {
			case errCh <- errors.New("token de acesso vazio"):
			default:
			}
			http.Error(w, "empty token", http.StatusBadRequest)
			return
		}

		select {
		case tokenCh <- tok:
		default:
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	srv.Handler = withDarkPage(mux)
	go func() { _ = srv.ListenAndServe() }()
	defer func() {
		c2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c2)
		cancel()
	}()

	authURL := fmt.Sprintf("https://anilist.co/api/v2/oauth/authorize?client_id=%s&response_type=token",
		clientID)

	logAppend("oauth(implicit): opening ", authURL)
	openBrowser(authURL)

	loginTimeout := 2 * time.Minute
	checkInterval := 5 * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	timeoutTimer := time.NewTimer(loginTimeout)
	defer timeoutTimer.Stop()

	var accessToken string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			return err

		case accessToken = <-tokenCh:
			if strings.TrimSpace(accessToken) == "" {
				return errors.New("empty access token")
			}
			logAppend("oauth(implicit): got token len=", fmt.Sprint(len(accessToken)))
			goto success

		case <-timeoutTimer.C:
			responseMu.Lock()
			gotResponse := receivedResponse
			responseMu.Unlock()

			if !gotResponse {
				logAppend("oauth(implicit): timeout - user likely closed the browser")
				return errors.New("login cancelado ou expirou")
			}
			return errors.New("tempo esgotado na autenticação")

		case <-ticker.C:
		}
	}

success:
	name, uid, whoErr := whoAmI(ctx, accessToken)
	if whoErr != nil {
		logAppend("oauth: whoAmI error: ", whoErr.Error())
		return fmt.Errorf("não foi possível verificar o token: %w", whoErr)
	}

	store.mu.Lock()
	store.AccessToken = accessToken
	store.Username = name
	store.UserID = uid
	store.ShowAniProfile = true
	store.mu.Unlock()
	store.Save()

	logAppend("oauth: success; logged in as ", name, " (id ", fmt.Sprint(uid), ")")
	return nil
}

func whoAmI(ctx context.Context, token string) (username string, userID int, err error) {
	const q = `query{ Viewer { id name } }`
	body := map[string]any{"query": q}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return "", 0, fmt.Errorf("whoAmI http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data struct {
			Viewer struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"Viewer"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		return out.Data.Viewer.Name, out.Data.Viewer.ID, nil
	}
	return "", 0, errors.New("decode viewer failed")
}

func randBase64URL(n int) string {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randAlphaNum(n int) string {
	if n <= 0 {
		n = 64
	}
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("a", n)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = chars[int(b[i])%len(chars)]
	}
	return string(out)
}

type malTokenResp struct {
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func malExchangeCode(ctx context.Context, cfg Config, code string, codeVerifier string) (malTokenResp, error) {
	var out malTokenResp
	code = strings.TrimSpace(code)
	codeVerifier = strings.TrimSpace(codeVerifier)
	if cfg.MALClientID == "" {
		return out, errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}
	if strings.TrimSpace(cfg.MALClientSecret) == "" {
		return out, errors.New("missing MAL client secret")
	}
	if code == "" {
		return out, errors.New("missing code")
	}
	if codeVerifier == "" {
		return out, errors.New("missing code_verifier")
	}

	v := url.Values{}
	v.Set("client_id", cfg.MALClientID)
	v.Set("client_secret", cfg.MALClientSecret)
	v.Set("grant_type", "authorization_code")
	v.Set("code", code)
	v.Set("code_verifier", codeVerifier)

	req, _ := http.NewRequestWithContext(ctx, "POST", malOAuthTokenURL, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("mal token http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	out.AccessToken = strings.TrimSpace(out.AccessToken)
	out.RefreshToken = strings.TrimSpace(out.RefreshToken)
	if out.AccessToken == "" {
		return out, errors.New("mal: empty access_token")
	}
	return out, nil
}

func malRefreshAccessToken(ctx context.Context, cfg Config, refreshToken string) (malTokenResp, error) {
	var out malTokenResp
	refreshToken = strings.TrimSpace(refreshToken)
	if cfg.MALClientID == "" {
		return out, errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}
	if strings.TrimSpace(cfg.MALClientSecret) == "" {
		return out, errors.New("missing MAL client secret")
	}
	if refreshToken == "" {
		return out, errors.New("missing refresh token")
	}

	v := url.Values{}
	v.Set("client_id", cfg.MALClientID)
	v.Set("client_secret", cfg.MALClientSecret)
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", refreshToken)

	req, _ := http.NewRequestWithContext(ctx, "POST", malOAuthTokenURL, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("mal refresh http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	out.AccessToken = strings.TrimSpace(out.AccessToken)
	out.RefreshToken = strings.TrimSpace(out.RefreshToken)
	if out.AccessToken == "" {
		return out, errors.New("mal: empty access_token")
	}
	return out, nil
}

func malWhoAmI(ctx context.Context, cfg Config, accessToken string) (name string, userID int, err error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", 0, errors.New("empty access token")
	}
	if strings.TrimSpace(cfg.MALClientID) == "" {
		return "", 0, errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}

	u, _ := url.Parse(malAPIBaseURL + "/users/@me")
	q := u.Query()
	q.Set("fields", "id,name")
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-MAL-CLIENT-ID", cfg.MALClientID)
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("mal whoami http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(out.Name), out.ID, nil
}

func malOAuthLogin(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.MALClientID) == "" {
		return errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}
	if strings.TrimSpace(cfg.MALClientSecret) == "" {
		return errors.New("missing MAL client secret")
	}
	redir := strings.TrimSpace(cfg.MALRedirectURI)
	if redir == "" {
		redir = "http://" + localMALLoginAddr + "/callback"
		cfg.MALRedirectURI = redir
	}

	u, err := url.Parse(redir)
	if err != nil {
		return fmt.Errorf("invalid MAL redirect uri: %w", err)
	}
	addr := strings.TrimSpace(u.Host)
	if addr == "" {
		addr = localMALLoginAddr
	}
	path := strings.TrimSpace(u.Path)
	if path == "" {
		path = "/callback"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	state := randBase64URL(12)
	// 48 random bytes -> 64 base64url chars, comfortably inside MAL's 43-128
	// range. The previous 96 bytes were truncated to exactly 128, which some
	// OAuth servers (MAL included) reject at the verifier-check step.
	verifier := randBase64URL(96)
	if len(verifier) > 128 {
		verifier = verifier[:128]
	}
	if len(verifier) < 43 {
		verifier = verifier + strings.Repeat("a", 43-len(verifier))
	}
	challenge := verifier

	// SO_REUSEADDR lets a new login bind even when a previous attempt left the
	// port in TIME_WAIT (Windows holds it for a while after close). Without it a
	// second "Entrar" click, or a login right after a cancelled one, fails with
	// WSAEADDRINUSE even though nothing is actually listening.
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// SO_REUSEADDR on Windows also permits rebinding to a port in
				// TIME_WAIT, which is exactly the case we need.
				_ = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
			})
		},
	}
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("could not listen for MAL callback on %s: %w", addr, err)
	}
	defer ln.Close()

	resultCh := make(chan error, 1)
	var once sync.Once
	sendResult := func(e error) {
		once.Do(func() {
			resultCh <- e
		})
	}

	srv := &http.Server{}
	mux := http.NewServeMux()

	var cbMu sync.Mutex
	cbInFlight := false
	cbDone := false

	successHTML := `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Login do MAL</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Login realizado com sucesso</h2>
<p>Você pode fechar esta aba agora.</p>
<script>setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 200);</script>
</body></html>`

	processingHTML := `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Login do MAL</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Concluindo login…</h2>
<p>Volte ao Kouki em alguns instantes.</p>
</body></html>`

	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		cbMu.Lock()
		if cbDone {
			cbMu.Unlock()
			io.WriteString(w, successHTML)
			return
		}
		if cbInFlight {
			cbMu.Unlock()
			io.WriteString(w, processingHTML)
			return
		}
		cbInFlight = true
		cbMu.Unlock()
		defer func() {
			cbMu.Lock()
			if !cbDone {
				cbInFlight = false
			}
			cbMu.Unlock()
		}()

		q := r.URL.Query()
		if e := strings.TrimSpace(q.Get("error")); e != "" {
			sendResult(fmt.Errorf("mal oauth error: %s", e))
			io.WriteString(w, "Falha no login. Você pode fechar esta aba.")
			return
		}
		if gotState := strings.TrimSpace(q.Get("state")); gotState != state {
			sendResult(errors.New("mal oauth: state mismatch"))
			io.WriteString(w, "Falha no login (estado incompatível). Você pode fechar esta aba.")
			return
		}
		code := strings.TrimSpace(q.Get("code"))
		if code == "" {
			sendResult(errors.New("mal oauth: missing code"))
			io.WriteString(w, "Falha no login (código ausente). Você pode fechar esta aba.")
			return
		}

		c, cancel := context.WithTimeout(ctx, 25*time.Second)
		tr, err := malExchangeCode(c, cfg, code, verifier)
		cancel()
		if err != nil {
			sendResult(err)
			io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Login do MAL</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Falha no login</h2>
<p>Não foi possível concluir a troca do token.</p>
<p><small>`+html.EscapeString(err.Error())+`</small></p>
</body></html>`)
			return
		}

		expiresAt := time.Now().Unix() + int64(tr.ExpiresIn)
		name := ""
		uid := 0
		c2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
		if n, id, e := malWhoAmI(c2, cfg, tr.AccessToken); e == nil {
			name, uid = n, id
		}
		cancel2()

		store.mu.Lock()
		store.MALAccessToken = tr.AccessToken
		store.MALRefreshToken = tr.RefreshToken
		store.MALTokenExpiresAt = expiresAt
		store.MALUsername = name
		store.MALUserID = uid
		store.mu.Unlock()
		store.Save()

		logAppend("mal oauth: success; logged in as ", firstNonEmpty(name, "<unknown>"))

		cbMu.Lock()
		cbDone = true
		cbMu.Unlock()

		sendResult(nil)
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Kouki · Login do MAL</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Login realizado com sucesso</h2>
<p>Você pode fechar esta aba agora.</p>
<script>setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 200);</script>
</body></html>`)
	})

	srv.Handler = withDarkPage(mux)
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		c2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c2)
		cancel()
	}()

	qs := url.Values{}
	qs.Set("response_type", "code")
	qs.Set("client_id", cfg.MALClientID)
	qs.Set("state", state)
	qs.Set("code_challenge", challenge)
	authURL := malOAuthAuthURL + "?" + qs.Encode()

	logAppend("mal oauth: opening ", authURL)
	openBrowser(authURL)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case e := <-resultCh:
		return e
	}
}

func malGetAccessToken(ctx context.Context, cfg Config) (string, error) {
	store.mu.RLock()
	accessToken := strings.TrimSpace(store.MALAccessToken)
	refreshToken := strings.TrimSpace(store.MALRefreshToken)
	expiresAt := store.MALTokenExpiresAt
	store.mu.RUnlock()

	if accessToken == "" {
		return "", errors.New("not signed in to MyAnimeList")
	}

	now := time.Now().Unix()
	if expiresAt == 0 || now < expiresAt-60 {
		return accessToken, nil
	}
	if refreshToken == "" {
		return accessToken, nil
	}

	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	tr, err := malRefreshAccessToken(c, cfg, refreshToken)
	cancel()
	if err != nil {
		return "", err
	}

	expiresAt2 := time.Now().Unix() + int64(tr.ExpiresIn)

	store.mu.Lock()
	store.MALAccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		store.MALRefreshToken = tr.RefreshToken
	}
	store.MALTokenExpiresAt = expiresAt2
	store.mu.Unlock()
	store.Save()

	return tr.AccessToken, nil
}

func syncMALProgress(ctx context.Context, cfg Config, animeID int, progress int, totalEps int) error {
	if strings.TrimSpace(cfg.MALClientID) == "" {
		return errors.New("missing MAL client id (set MAL_CLIENT_ID)")
	}
	if animeID <= 0 || progress <= 0 {
		return errors.New("invalid sync parameters")
	}
	if totalEps == 1 && progress > 1 {
		progress = 1
	}
	if totalEps > 0 && progress > totalEps {
		progress = totalEps
	}

	accessToken, err := malGetAccessToken(ctx, cfg)
	if err != nil {
		return err
	}

	status := "watching"
	if totalEps > 0 && progress >= totalEps {
		status = "completed"
	}

	v := url.Values{}
	v.Set("num_watched_episodes", strconv.Itoa(progress))
	v.Set("status", status)

	endpoint := fmt.Sprintf("%s/anime/%d/my_list_status", malAPIBaseURL, animeID)
	req, _ := http.NewRequestWithContext(ctx, "PATCH", endpoint, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-MAL-CLIENT-ID", cfg.MALClientID)
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mal sync http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func refreshMALViewerFromToken(ctx context.Context, cfg Config) {
	store.mu.RLock()
	tok := strings.TrimSpace(store.MALAccessToken)
	store.mu.RUnlock()
	if tok == "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	name, uid, err := malWhoAmI(c, cfg, tok)
	if err != nil {
		logAppend("mal viewer refresh failed: ", err.Error())
		return
	}

	store.mu.Lock()
	if strings.TrimSpace(store.MALAccessToken) != tok {
		store.mu.Unlock()
		return
	}
	changed := store.MALUsername != name || store.MALUserID != uid
	store.MALUsername = name
	store.MALUserID = uid
	store.mu.Unlock()
	if changed {
		store.Save()
		refreshAuthMenu()
	}
}

func malTrackingEnabled() bool {
	store.mu.RLock()
	on := store.MALTracking
	hasToken := strings.TrimSpace(store.MALAccessToken) != ""
	store.mu.RUnlock()
	return on && hasToken
}

type mediaListEntryLite struct {
	ID       int    `json:"id"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
}

func getMediaListEntry(ctx context.Context, token string, userID int, mediaID int) (entry *mediaListEntryLite, err error) {
	const q = `
query($userId:Int, $mediaId:Int) {
  MediaList(userId:$userId, mediaId:$mediaId, type: ANIME) {
    id
    status
    progress
  }
}`
	vars := map[string]any{"userId": userID, "mediaId": mediaID}
	body := map[string]any{"query": q, "variables": vars}
	bs, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return nil, fmt.Errorf("MediaList http %d: %s", resp.StatusCode, string(b))
	}

	var out struct {
		Data struct {
			MediaList *mediaListEntryLite `json:"MediaList"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data.MediaList, nil
}

func saveMediaListEntry(ctx context.Context, token string, mediaID int, progress int, status string) error {
	const m = `
mutation($mediaId:Int, $progress:Int, $status:MediaListStatus) {
  SaveMediaListEntry(mediaId:$mediaId, progress:$progress, status:$status) { id status progress }
}`
	vars := map[string]any{"mediaId": mediaID, "progress": progress, "status": status}
	body := map[string]any{"query": m, "variables": vars}
	bs, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return fmt.Errorf("SaveMediaListEntry http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func syncAniListProgress(ctx context.Context, token string, userID int, mediaID int, progress int, totalEps int) error {
	if strings.TrimSpace(token) == "" || userID <= 0 || mediaID <= 0 || progress <= 0 {
		return errors.New("invalid sync parameters")
	}
	if totalEps == 1 && progress > 1 {
		progress = 1
	}
	if totalEps > 0 && progress > totalEps {
		progress = totalEps
	}

	var existingStatus string
	if e, err := getMediaListEntry(ctx, token, userID, mediaID); err == nil && e != nil {
		existingStatus = strings.TrimSpace(e.Status)
	}

	targetStatus := aniStatusCurrent
	switch strings.ToUpper(existingStatus) {
	case aniStatusCompleted:
		targetStatus = aniStatusRepeating
	case aniStatusRepeating:
		targetStatus = aniStatusRepeating
	case aniStatusCurrent:
		targetStatus = aniStatusCurrent
	case "":
		targetStatus = aniStatusCurrent
	default:
		targetStatus = aniStatusCurrent
	}

	if totalEps > 0 && progress >= totalEps {
		targetStatus = aniStatusCompleted
	}

	return saveMediaListEntry(ctx, token, mediaID, progress, targetStatus)
}

func refreshViewerFromToken(ctx context.Context) {
	store.mu.RLock()
	tok := strings.TrimSpace(store.AccessToken)
	store.mu.RUnlock()
	if tok == "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	name, uid, err := whoAmI(c, tok)
	if err != nil {
		logAppend("viewer refresh failed: ", err.Error())
		return
	}

	store.mu.Lock()
	if strings.TrimSpace(store.AccessToken) != tok {
		store.mu.Unlock()
		return
	}
	changed := store.Username != name || store.UserID != uid
	store.Username = name
	store.UserID = uid
	store.mu.Unlock()
	if changed {
		store.Save()
		refreshAuthMenu()
	}
}

func anilistTrackingEnabled() bool {
	store.mu.RLock()
	on := store.AniListTracking
	hasToken := strings.TrimSpace(store.AccessToken) != ""
	uid := store.UserID
	store.mu.RUnlock()
	return on && hasToken && uid > 0
}

func onReadyTray(ctx context.Context, cancel context.CancelFunc) {
	setTrayIcon()
	systray.SetTooltip("Kouki")

	menuShowWindow = systray.AddMenuItem("Abrir o "+appName, "Mostrar a janela principal")
	systray.AddSeparator()

	menuLogin = systray.AddMenuItem("Entrar no AniList…", "Autenticar este dispositivo no AniList")
	menuLogout = systray.AddMenuItem("Sair do AniList", "Esquecer o token salvo do AniList")
	menuCorrectAnime = systray.AddMenuItem("Selecionar anime correto…", "Selecionar manualmente o anime do AniList para o arquivo atual")
	menuCorrectAnime.Disable()
	menuAniListTrack = systray.AddMenuItemCheckbox(
		"Ativar acompanhamento do AniList",
		"Define se o Kouki atualiza seu progresso no AniList enquanto você assiste",
		store.AniListTracking,
	)
	menuFillerWarn = systray.AddMenuItemCheckbox(
		"Avisar sobre episódios filler",
		"Mostrar um aviso ao assistir episódios filler (quando disponível)",
		store.WarnFiller,
	)
	systray.AddSeparator()

	menuMALLogin = systray.AddMenuItem("Entrar no MyAnimeList…", "Autenticar este dispositivo no MyAnimeList")
	menuMALLogout = systray.AddMenuItem("Sair do MyAnimeList", "Esquecer o token salvo do MyAnimeList")
	menuMALCorrectAnime = systray.AddMenuItem("Selecionar anime correto (MAL)…", "Selecionar manualmente o anime do MyAnimeList para o arquivo atual")
	menuMALCorrectAnime.Disable()
	menuMALTrack = systray.AddMenuItemCheckbox(
		"Ativar acompanhamento do MyAnimeList",
		"Define se o Kouki atualiza seu progresso no MyAnimeList enquanto você assiste",
		store.MALTracking,
	)
	systray.AddSeparator()

	menuSimul = systray.AddMenuItem("Simulwatching", "Hospedar ou entrar em uma sessão de Simulwatching")
	menuSimulHost = menuSimul.AddSubMenuItem("Hospedar uma sessão…", "Hospedar uma sessão e compartilhar seu estado de reprodução")
	menuSimulJoin = menuSimul.AddSubMenuItem("Entrar em uma sessão…", "Entrar na sessão hospedada por um amigo")
	menuSimulStop = menuSimul.AddSubMenuItem("Parar o Simulwatching", "Parar de hospedar ou participar")
	menuSimulStatus = menuSimul.AddSubMenuItem("Status: Desligado", "Mostra o estado atual do Simulwatching")
	menuSimulStatus.Disable()
	store.mu.RLock()
	joinSync := store.SimulJoinSync
	store.mu.RUnlock()
	menuSimulJoinSync = menuSimul.AddSubMenuItemCheckbox(
		"Sincronizar meu AniList",
		"Quando conectado, sincroniza seu AniList com base nos eventos de 80% de progresso do anfitrião",
		joinSync,
	)
	simulLoadSettingsFromStore()
	simulRefreshTray()
	systray.AddSeparator()

	menuRunOnStartup = systray.AddMenuItemCheckbox(
		"Executar ao iniciar o Windows",
		"Iniciar o Kouki automaticamente ao entrar no Windows",
		store.RunOnStartup,
	)
	systray.AddSeparator()

	menuOpenFolder = systray.AddMenuItem("Abrir pasta do programa", "Abrir a pasta onde o Kouki está instalado")
	menuAbout = systray.AddMenuItem("Sobre o "+appName+" "+appVersion, "Versão, caminhos e informações do programa")
	systray.AddSeparator()
	menuQuit = systray.AddMenuItem("Sair", "Fechar o Kouki")

	refreshAuthMenu()

	go refreshViewerFromToken(ctx)
	go refreshMALViewerFromToken(ctx, loadConfig())

	go func() {
		for {
			select {
			case <-menuLogin.ClickedCh:
				loginCtx, generation, canceled := startOrCancelAniLogin(ctx)
				refreshAuthMenu()
				if canceled {
					continue
				}

				go func() {
					err := oauthLogin(loginCtx)
					finishAniLogin(generation)

					if err != nil {
						if err == context.Canceled {
							fmt.Println("AniList login cancelled by user")
							logAppend("AniList login cancelled by user")
						} else {
							fmt.Println("AniList login failed:", err)
							logAppend("AniList login failed:", err.Error())
						}
					} else {
						fmt.Println("AniList login succeeded")
					}

					store.Load()
					refreshAuthMenu()
				}()

			case <-menuLogout.ClickedCh:
				store.ClearAniList()
				refreshAuthMenu()

			case <-menuAniListTrack.ClickedCh:
				store.mu.Lock()
				store.AniListTracking = !store.AniListTracking
				on := store.AniListTracking
				hasToken := store.AccessToken != ""
				store.mu.Unlock()
				if on {
					menuAniListTrack.Check()
				} else {
					menuAniListTrack.Uncheck()
				}
				store.Save()
				if !hasToken {
					menuAniListTrack.Disable()
				}

			case <-menuMALLogin.ClickedCh:
				cfg2 := loadConfig()
				if strings.TrimSpace(cfg2.MALClientID) == "" || strings.TrimSpace(cfg2.MALClientSecret) == "" {
					messageBox("Kouki: MyAnimeList", malCredentialsHint, mbOK|mbIconError)
					continue
				}

				loginCtx, generation, canceled := startOrCancelMALLogin(ctx)
				refreshAuthMenu()
				if canceled {
					continue
				}

				go func() {
					err := malOAuthLogin(loginCtx, cfg2)
					finishMALLogin(generation)

					if err != nil {
						if err == context.Canceled {
							fmt.Println("MAL login cancelled by user")
							logAppend("MAL login cancelled by user")
						} else {
							fmt.Println("MAL login failed:", err)
							logAppend("MAL login failed:", err.Error())
							messageBox("Kouki: MyAnimeList", "Falha no login do MyAnimeList:\n\n"+err.Error(), mbOK|mbIconError)
						}
					} else {
						fmt.Println("MAL login succeeded")
					}

					store.Load()
					refreshAuthMenu()
				}()

			case <-menuMALLogout.ClickedCh:
				store.ClearMAL()
				refreshAuthMenu()

			case <-menuMALTrack.ClickedCh:
				store.mu.Lock()
				store.MALTracking = !store.MALTracking
				on := store.MALTracking
				hasToken := strings.TrimSpace(store.MALAccessToken) != ""
				store.mu.Unlock()
				if on {
					menuMALTrack.Check()
				} else {
					menuMALTrack.Uncheck()
				}
				store.Save()
				if !hasToken {
					menuMALTrack.Disable()
				}

			case <-menuFillerWarn.ClickedCh:
				store.mu.Lock()
				store.WarnFiller = !store.WarnFiller
				on := store.WarnFiller
				store.mu.Unlock()
				if on {
					menuFillerWarn.Check()
				} else {
					menuFillerWarn.Uncheck()
				}
				store.Save()

			case <-menuRunOnStartup.ClickedCh:
				store.mu.Lock()
				store.RunOnStartup = !store.RunOnStartup
				on := store.RunOnStartup
				store.mu.Unlock()
				if on {
					menuRunOnStartup.Check()
				} else {
					menuRunOnStartup.Uncheck()
				}
				store.Save()
				if err := setWindowsRunOnStartup(on); err != nil {
					logAppend("startup: failed to update HKCU Run entry: ", err.Error())
					messageBox("Kouki: falha na configuração de inicialização", "Não foi possível atualizar a configuração de inicialização do Windows:\n\n"+err.Error(), mbOK|mbIconError)
				}

			case <-menuSimulJoinSync.ClickedCh:
				store.mu.Lock()
				store.SimulJoinSync = !store.SimulJoinSync
				on := store.SimulJoinSync
				store.mu.Unlock()
				if on {
					menuSimulJoinSync.Check()
				} else {
					menuSimulJoinSync.Uncheck()
				}
				store.Save()

			case <-menuSimulHost.ClickedCh:
				go func() {
					_ = simulStartHostUI(ctx, func(code string) {
						simul.mu.Lock()
						simul.code = code
						simul.mu.Unlock()
						go simulStartHost(ctx)
					})
				}()

			case <-menuSimulJoin.ClickedCh:
				go func() {
					_ = simulStartJoinUI(ctx, loadConfig().UserAgent, func(addr, code string) {
						go simulStartJoin(ctx, addr, code)
					})
				}()

			case <-menuSimulStop.ClickedCh:
				simulStop()

			case <-menuCorrectAnime.ClickedCh:
				curTrack.mu.RLock()
				active := curTrack.Active
				seriesKey := curTrack.SeriesKey
				q := cleanTitleForSearch(firstNonEmpty(curTrack.ParsedTitle, curTrack.MediaTitle))
				curTrack.mu.RUnlock()
				if !active || seriesKey == "" {
					continue
				}
				go func(seriesKey string, initialQ string) {
					_ = startAniListSelector(initialQ, loadConfig().UserAgent, func(id int) {
						overrides.Set(seriesKey, id)
						select {
						case resolveCh <- resolveRequest{SeriesKey: seriesKey, MediaID: id}:
						default:
						}
					})
				}(seriesKey, q)

			case <-menuMALCorrectAnime.ClickedCh:
				curTrack.mu.RLock()
				active := curTrack.Active
				seriesKey := curTrack.SeriesKey
				q := cleanTitleForSearch(firstNonEmpty(curTrack.ParsedTitle, curTrack.MediaTitle))
				curTrack.mu.RUnlock()
				if !active || seriesKey == "" {
					continue
				}
				cfg2 := loadConfig()
				if strings.TrimSpace(cfg2.MALClientID) == "" {
					messageBox("Kouki: MyAnimeList", "ID do cliente MAL ausente.\n\nDefina a variável de ambiente MAL_CLIENT_ID e tente novamente.", mbOK|mbIconError)
					continue
				}
				go func(seriesKey string, initialQ string) {
					_ = startMALSelector(initialQ, cfg2.MALClientID, cfg2.UserAgent, func(id int) {
						malOverrides.Set(seriesKey, id)
						select {
						case resolveMALCh <- resolveMALRequest{SeriesKey: seriesKey, AnimeID: id}:
						default:
						}
					})
				}(seriesKey, q)

			case <-menuShowWindow.ClickedCh:
				showMainWindow()

			case <-menuOpenFolder.ClickedCh:
				openInstallFolder()

			case <-menuAbout.ClickedCh:
				showAboutDialog()

			case <-menuQuit.ClickedCh:
				loginCancelMu.Lock()
				if loginCancelFunc != nil {
					loginCancelFunc()
					loginCancelFunc = nil
				}
				loginCancelMu.Unlock()

				malLoginCancelMu.Lock()
				if malLoginCancelFunc != nil {
					malLoginCancelFunc()
					malLoginCancelFunc = nil
				}
				malLoginCancelMu.Unlock()

				cancel()
				requestQuit()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func runWithTray(mainfn func(context.Context), onExit func()) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	systray.Run(func() {
		onReadyTray(ctx, cancel)

		go func() {
			defer cancel()
			mainfn(ctx)
		}()

		go func() {
			<-ctx.Done()
			systray.Quit()
		}()
	}, func() {
		if onExit != nil {
			onExit()
		}
	})
}

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK           = 0x00000000
	mbOKCancel     = 0x00000001
	mbYesNo        = 0x00000004
	mbIconInfo     = 0x00000040
	mbIconWarning  = 0x00000030
	mbIconQuestion = 0x00000020
	mbIconError    = 0x00000010

	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000

	idOK  = 1
	idYes = 6
)

func messageBox(title, text string, style uint32) int {
	style |= mbSetForeground | mbTopMost

	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)

	r, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(t)),
		uintptr(unsafe.Pointer(c)),
		uintptr(style),
	)

	return int(r)
}

// main is the entry point for BOTH the bootstrapper and the installed
// application. The same binary acts as installer until it has been installed.
//
// Decision table (see install.go):
//
//	valid install + we ARE the installed copy  -> run the tray app
//	valid install + we are a stray bootstrapper -> hand off, then exit
//	no valid install                            -> run the installer, never the tray
func main() {
	if len(os.Args) == 2 && (strings.EqualFold(os.Args[1], "--version") || strings.EqualFold(os.Args[1], "-version")) {
		fmt.Printf("%s %s (%s/%s)\n", appName, appVersion, runtime.GOOS, runtime.GOARCH)
		return
	}

	release, err := acquireSingleInstanceGuard()
	if err != nil {
		// Another copy already owns the tray icon and the main window; exit quietly
		// so double-clicking the shortcut cannot spawn duplicates.
		return
	}
	defer release()

	for _, a := range os.Args[1:] {
		if strings.EqualFold(a, startHiddenArg) {
			startHidden = true
		}
	}

	// Migration comes BEFORE anything is loaded. It used to run afterwards, so on
	// the first start the migrated tokens were copied to disk but never read: the
	// window showed the user signed out, and the first save wrote the empty
	// in-memory settings over the migrated file.
	//
	// One-way migration from the previous release, then drop its autostart value
	// so the machine does not end up launching both products.
	if err := MigrateLegacyConfig(); err != nil {
		logAppend("startup: legacy migration: ", err.Error())
	}
	_ = removeLegacyRunKey()

	cfg := loadConfig()
	store.Load()
	loadZeroBasedSignatures()
	overrides.Load()
	malOverrides.Load()

	// Re-assert the autostart entry so it always points at the binary the user
	// actually installed, not a stale copy left over in Downloads.
	store.mu.RLock()
	autostart := store.RunOnStartup
	store.mu.RUnlock()
	if !autostart && windowsRunValueExists() {
		// The entry exists but the settings say "off": the installer's autostart
		// task (or the user) turned it on from outside the app. Adopt it instead
		// of deleting it, which is what used to make that installer checkbox do
		// nothing at all. Switching it off in the window removes the entry.
		store.mu.Lock()
		store.RunOnStartup = true
		store.mu.Unlock()
		store.Save()
		autostart = true
	}
	if autostart {
		if err := setWindowsRunOnStartup(true); err != nil {
			logAppend("startup: falha ao aplicar a inicialização automática: ", err.Error())
		}
	}

	// Make sure Kouki itself is reachable from the Start Menu and the Run dialog
	// even when the user ran the bare executable instead of the installer. Both
	// calls are idempotent and per-user, so they never need elevation.
	ensureKoukiStartMenu()
	ensureKoukiAppPaths()

	// The tray owns the notification-area icon and the quick actions. It runs on
	// its own locked OS thread so its GetMessage loop is never migrated by the Go
	// runtime, and so it never contends with walk's loop on the main thread.
	go func() {
		runtime.LockOSThread()
		runWithTray(func(ctx context.Context) { run(ctx, cfg) }, nil)
	}()

	// The main window owns the main thread and blocks until the user quits.
	runMainWindow()
}

type fillerInfo struct {
	Slug       string
	FillerEps  map[int]bool
	LastLookup time.Time
}

var (
	fillerCache = struct {
		mu      sync.Mutex
		byAniID map[int]*fillerInfo
	}{
		byAniID: make(map[int]*fillerInfo),
	}
	fillerWarned = struct {
		mu  sync.Mutex
		set map[string]bool
	}{
		set: make(map[string]bool),
	}
	fillerRowRe = regexp.MustCompile(`(?s)<tr class="filler[^"]*"[^>]*>.*?<td class="Number">(\d+)</td>`)
)

func levenshtein(a, b string) int {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	na, nb := len(a), len(b)
	if na == 0 {
		return nb
	}
	if nb == 0 {
		return na
	}
	dp := make([]int, (na+1)*(nb+1))
	idx := func(i, j int) int { return i*(nb+1) + j }

	for i := 0; i <= na; i++ {
		dp[idx(i, 0)] = i
	}
	for j := 0; j <= nb; j++ {
		dp[idx(0, j)] = j
	}

	for i := 1; i <= na; i++ {
		for j := 1; j <= nb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			del := dp[idx(i-1, j)] + 1
			ins := dp[idx(i, j-1)] + 1
			sub := dp[idx(i-1, j-1)] + cost
			dp[idx(i, j)] = minInt(del, minInt(ins, sub))
		}
	}
	return dp[idx(na, nb)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type fillerSearchResult struct {
	Slug  string
	Title string
}

func fetchFillerShows(ctx context.Context) ([]fillerSearchResult, error) {
	results := []fillerSearchResult{}

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.animefillerlist.com/shows", nil)
	req.Header.Set("User-Agent", "kouki-filler-check/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return nil, fmt.Errorf("animefillerlist shows http %d: %s", resp.StatusCode, string(body))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return nil, err
	}
	htmlStr := string(b)

	needle := `<a href="/shows/`
	for {
		i := strings.Index(htmlStr, needle)
		if i == -1 {
			break
		}
		htmlStr = htmlStr[i+len(`<a href="`):]
		j := strings.Index(htmlStr, `"`)
		if j == -1 {
			break
		}
		href := htmlStr[:j]
		htmlStr = htmlStr[j+2:]

		k := strings.Index(htmlStr, "</a>")
		if k == -1 {
			break
		}
		title := htmlStr[:k]
		title = html.UnescapeString(strings.TrimSpace(title))

		if strings.HasPrefix(href, "/shows/") && title != "" {
			results = append(results, fillerSearchResult{
				Slug:  href,
				Title: title,
			})
		}

		htmlStr = htmlStr[k+4:]
	}

	if len(results) == 0 {
		return nil, errors.New("no shows parsed from animefillerlist")
	}
	return results, nil
}

func searchFillerSlug(ctx context.Context, titles []string) (string, error) {
	shows, err := fetchFillerShows(ctx)
	if err != nil {
		return "", err
	}

	norm := func(s string) string {
		return strings.ToLower(strings.TrimSpace(cleanTitleForSearch(s)))
	}
	tokenSet := func(s string) map[string]bool {
		s = norm(s)
		out := make(map[string]bool)
		for _, w := range strings.Fields(s) {
			w = strings.TrimSpace(w)
			if len(w) < 3 {
				continue
			}
			out[w] = true
		}
		return out
	}
	overlapCount := func(a, b map[string]bool) int {
		n := 0
		for k := range a {
			if b[k] {
				n++
			}
		}
		return n
	}

	type candidate struct {
		Slug     string
		Title    string
		Distance int
	}

	var cands []candidate
	for _, s := range shows {
		baseTitle := s.Title

		secondTitle := ""
		if start := strings.LastIndex(baseTitle, " ("); start != -1 && strings.HasSuffix(baseTitle, ")") {
			secondTitle = baseTitle[start+2 : len(baseTitle)-1]
			baseTitle = baseTitle[:start]
		}

		allVariants := []string{baseTitle}
		if secondTitle != "" {
			allVariants = append(allVariants, secondTitle)
		}
		vTok := make([]map[string]bool, 0, len(allVariants))
		for _, v := range allVariants {
			vTok = append(vTok, tokenSet(v))
		}

		for _, t := range titles {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			tTok := tokenSet(t)
			qt := len(tTok)
			if qt == 0 {
				continue
			}
			for _, v := range allVariants {
				ov := 0
				for _, vv := range vTok {
					o := overlapCount(tTok, vv)
					if o > ov {
						ov = o
					}
				}
				if ov == 0 {
					continue
				}
				if qt >= 2 && ov*2 < qt {
					continue
				}

				dist := levenshtein(norm(t), norm(v))
				cands = append(cands, candidate{
					Slug:     s.Slug,
					Title:    v,
					Distance: dist,
				})
			}
		}
	}

	if len(cands) == 0 {
		return "", errors.New("no candidates for filler slug")
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Distance != cands[j].Distance {
			return cands[i].Distance < cands[j].Distance
		}
		return len(cands[i].Title) < len(cands[j].Title)
	})

	best := cands[0]
	if best.Distance > 10 {
		return "", errors.New("no good filler match (distance too high)")
	}

	return best.Slug, nil
}

func fetchFillerEpisodes(ctx context.Context, slug string) (map[int]bool, error) {
	if !strings.HasPrefix(slug, "/") {
		slug = "/shows/" + slug
	}
	fullURL := "https://www.animefillerlist.com" + slug

	req, _ := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	req.Header.Set("User-Agent", "kouki-filler-check/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Request != nil && resp.Request.URL != nil {
		got := resp.Request.URL.String()
		want := fullURL
		if strings.TrimSpace(got) != "" && strings.TrimSpace(want) != "" {
			if !strings.HasPrefix(got, want) {
				return nil, fmt.Errorf("animefillerlist show redirect/mismatch: got %s", got)
			}
		}
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		return nil, fmt.Errorf("animefillerlist show http %d: %s", resp.StatusCode, string(body))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return nil, err
	}
	htmlStr := string(b)
	if !strings.Contains(strings.ToLower(htmlStr), "animefillerlist") {
		return nil, errors.New("animefillerlist mismatch (unexpected page)")
	}

	matches := fillerRowRe.FindAllStringSubmatch(htmlStr, -1)
	filler := make(map[int]bool)

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		numStr := strings.TrimSpace(m[1])
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			filler[n] = true
		}
	}

	if len(filler) == 0 {
		return nil, errors.New("no filler episodes parsed from page")
	}
	return filler, nil
}

func getFillerInfo(ctx context.Context, aniID int, titles []string) (*fillerInfo, error) {
	fillerCache.mu.Lock()
	if fi, ok := fillerCache.byAniID[aniID]; ok {
		if fi.FillerEps == nil {
			if !fi.LastLookup.IsZero() && time.Since(fi.LastLookup) < 12*time.Hour {
				fillerCache.mu.Unlock()
				return nil, errors.New("no filler data for this show")
			}
		}
		if fi.FillerEps != nil {
			fillerCache.mu.Unlock()
			return fi, nil
		}
	}
	fillerCache.mu.Unlock()

	slug, err := searchFillerSlug(ctx, titles)
	if err != nil {
		fi := &fillerInfo{Slug: "", FillerEps: nil, LastLookup: time.Now()}
		fillerCache.mu.Lock()
		fillerCache.byAniID[aniID] = fi
		fillerCache.mu.Unlock()
		return nil, err
	}
	eps, err := fetchFillerEpisodes(ctx, slug)
	if err != nil {
		fi := &fillerInfo{Slug: slug, FillerEps: nil, LastLookup: time.Now()}
		fillerCache.mu.Lock()
		fillerCache.byAniID[aniID] = fi
		fillerCache.mu.Unlock()
		return nil, err
	}

	fi := &fillerInfo{
		Slug:       slug,
		FillerEps:  eps,
		LastLookup: time.Now(),
	}
	fillerCache.mu.Lock()
	fillerCache.byAniID[aniID] = fi
	fillerCache.mu.Unlock()
	return fi, nil
}

func markFillerWarned(aniID, ep int) bool {
	key := fmt.Sprintf("%d:%d", aniID, ep)
	fillerWarned.mu.Lock()
	defer fillerWarned.mu.Unlock()
	if fillerWarned.set[key] {
		return false
	}
	fillerWarned.set[key] = true
	return true
}

func fallbackTitleFrom(st mpvState) string {
	if t := strings.TrimSpace(st.MediaTitle); t != "" {
		return t
	}
	base := filepath.Base(strings.TrimSpace(st.FileName))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base
}

func run(ctx context.Context, cfg Config) {
	mgr := &discordManager{}
	defer mgr.close(cfg.DiscordAppID)
	go runSuwayomiRPC(ctx, cfg)

	if !discordRPCEnabled() {
		discordApplyActivity(cfg.DiscordAppID, mgr, nil)
	}

	joined := func() bool {
		simul.mu.Lock()
		on := simul.mode == simulModeJoin
		simul.mu.Unlock()
		return on
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if joined() {
			simulApplyJoinStateToUI(cfg, mgr)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		// sharedMpvConn publishes the live connection so the GUI can read
		// playback state without opening a second pipe.
		pipe := mpvPipeName()
		conn, err := dialMpv(pipe, 2*time.Second)
		if err != nil {
			sharedMpvConn.setErr(err)
			sharedMpvConn.set(nil)
			discordApplyActivity(cfg.DiscordAppID, mgr, nil)
			traySetIdle()
			// 600 ms, not 1.5 s: with the player closed this loop is what
			// notices a freshly launched one, and the old interval meant the GUI
			// could give up waiting long before the pipe was even looked for.
			time.Sleep(600 * time.Millisecond)
			continue
		}
		logAppend("mpv: canal IPC ativo em ", pipe)
		sharedMpvConn.set(conn)

		runLoop(ctx, cfg, conn, mgr)
		sharedMpvConn.set(nil)
		traySetIdle()
		time.Sleep(1 * time.Second)
	}
}

func runLoop(ctx context.Context, cfg Config, conn *mpvConn, mgr *discordManager) {
	defer conn.Close()

	cache := &presenceCache{}
	smooth := &progSmooth{}
	mpvTicker := time.NewTicker(cfg.PollInterval)
	uiTicker := time.NewTicker(1 * time.Second)
	var lastMpvUpdateAt time.Time
	defer mpvTicker.Stop()
	defer uiTicker.Stop()

	var currentFileKey string
	playing := false
	ready := false
	clearTrackingState()

	setPresence := func(details map[string]any) {
		discordApplyActivity(cfg.DiscordAppID, mgr, details)
	}

	joined := func() bool {
		simul.mu.Lock()
		on := simul.mode == simulModeJoin
		simul.mu.Unlock()
		return on
	}
	hosting := func() bool {
		simul.mu.Lock()
		on := simul.mode == simulModeHost
		simul.mu.Unlock()
		return on
	}
	setHostIdle := func() {
		if !hosting() {
			return
		}
		st := simulState{Ready: false, UpdatedAt: time.Now().UnixNano()}
		simul.mu.Lock()
		simul.hostState = st
		simul.mu.Unlock()
		simulBroadcastState(st)
	}
	broadcastHostState := func(hostFile string) {
		if !hosting() {
			return
		}
		cache.mu.Lock()
		aname := cache.lastAni
		ep := cache.lastEp
		cover := cache.lastCover
		aniID := cache.lastAniID
		totalEps := cache.lastTotalEps
		rewatch := cache.lastRewatch
		cache.mu.Unlock()

		var pos, dur float64
		var paused bool
		if ready {
			pos, dur, paused = smooth.estimate()
		} else {
			_, dur, paused = smooth.estimate()
			pos = 0
		}

		curTrack.mu.RLock()
		hs := curTrack.WantSeason
		curTrack.mu.RUnlock()

		st := simulState{
			Anime:      strings.TrimSpace(aname),
			Episode:    strings.TrimSpace(ep),
			AniID:      aniID,
			TotalEps:   totalEps,
			CoverURL:   strings.TrimSpace(cover),
			Rewatching: rewatch,
			Paused:     paused,
			Pos:        pos,
			Dur:        dur,
			Ready:      ready && playing,
			UpdatedAt:  time.Now().UnixNano(),
			HostFile:   strings.TrimSpace(hostFile),
			HostSeason: hs,
		}
		simul.mu.Lock()
		simul.hostState = st
		simul.mu.Unlock()
		simulBroadcastState(st)
	}

	pushEstimated := func() {
		if !playing {
			return
		}
		cache.mu.Lock()
		aname := cache.lastAni
		ep := cache.lastEp
		cover := cache.lastCover
		aniID := cache.lastAniID
		totalEps := cache.lastTotalEps
		rewatch := cache.lastRewatch
		cache.mu.Unlock()

		var pos, dur float64
		var paused bool
		if ready {
			pos, dur, paused = smooth.estimate()
		} else {
			_, dur, paused = smooth.estimate()
			pos = 0
		}

		episodeLabel := ""
		if strings.TrimSpace(ep) != "" {
			if totalEps > 0 {
				episodeLabel = fmt.Sprintf("%s/%d", ep, totalEps)
			} else {
				episodeLabel = fmt.Sprintf("%s/??", ep)
			}
		}

		act := buildActivity(aname, episodeLabel, "", cover, cfg.SmallImage, anilistURL(aniID), pos, dur, paused, rewatch)
		if !ready {
			delete(act, "timestamps")
		}
		setPresence(act)

		percent := -1
		if dur > 0 {
			p := int(pos/dur*100 + 0.5)
			if p < 0 {
				p = 0
			} else if p > 100 {
				p = 100
			}
			percent = p
		}
		traySetWatching(aname, ep, percent, totalEps)
	}

	// Broadcasts and remote progress updates have different success states.
	// A temporary API failure must not suppress progress for the rest of a file.
	reported := make(map[string]bool)
	aniSynced := make(map[string]bool)
	malSynced := make(map[string]bool)
	nextSyncAttempt := make(map[string]time.Time)

	for {
		select {
		case <-ctx.Done():
			return

		case rr := <-resolveCh:
			curTrack.mu.RLock()
			active := curTrack.Active
			seriesKey := curTrack.SeriesKey
			curTrack.mu.RUnlock()
			if active && seriesKey != "" && rr.SeriesKey == seriesKey {
				currentFileKey = ""
			}

		case rr := <-resolveMALCh:
			curTrack.mu.RLock()
			active := curTrack.Active
			seriesKey := curTrack.SeriesKey
			curTrack.mu.RUnlock()
			if active && seriesKey != "" && rr.SeriesKey == seriesKey {
				currentFileKey = ""
			}

		case <-mpvTicker.C:
			if joined() {
				simulApplyJoinStateToUI(cfg, mgr)
				continue
			}
			st, err := queryMpvState(conn)
			// Hand the window what was just read, so it never has to ask mpv.
			sharedMpvConn.publish(st, err == nil)
			if err != nil {
				if errors.Is(err, errNoFilePlaying) {
					if !conn.Alive() {
						setHostIdle()
						playing = false
						ready = false
						currentFileKey = ""
						smooth.reset()
						cache.clear()
						clearTrackingState()
						setPresence(nil)
						traySetIdle()
						return
					}

					setHostIdle()
					playing = false
					ready = false
					currentFileKey = ""
					smooth.reset()
					cache.clear()
					clearTrackingState()
					setPresence(nil)
					traySetIdle()
					continue
				}

				if isPipeGone(err) {
					logAppend("mpv: canal IPC encerrado; reconectando")
					setHostIdle()
					playing = false
					ready = false
					currentFileKey = ""
					smooth.reset()
					cache.clear()
					clearTrackingState()
					setPresence(nil)
					traySetIdle()
					return
				}

				logAppend("mpv: erro ao ler o estado: ", err.Error())
				setHostIdle()
				playing = false
				ready = false
				currentFileKey = ""
				smooth.reset()
				cache.clear()
				clearTrackingState()
				setPresence(nil)
				traySetIdle()
				return
			}

			playing = true
			if st.Duration > 0 {
				ready = true
			}
			smooth.updateFromMPV(st.TimePos, st.Duration, st.Pause)
			lastMpvUpdateAt = time.Now()

			key := st.FileName
			if key == "" {
				key = st.MediaTitle
			}
			if key != "" && key != currentFileKey {
				lowerKey := strings.ToLower(strings.TrimSpace(key))
				if lowerKey == "mpv" || strings.HasPrefix(lowerKey, "mpv v") {
					playing = false
					ready = false
					currentFileKey = ""
					smooth.reset()
					cache.clear()
					clearTrackingState()
					setPresence(nil)
					traySetIdle()
					continue
				}

				currentFileKey = key
				ready = st.Duration > 0

				md := habari.Parse(key)
				title, ep := pickEpisode(md, fallbackTitleFrom(st))
				ep = applyZeroBasedEpisodeFix(firstNonEmpty(st.Path, st.FileName), key, ep)
				wantYear := wantYearFrom(md, key, st.MediaTitle)
				wantSeason := wantSeasonFrom(key, title, st.MediaTitle)
				seriesKey := seriesKeyForOverride(key, title, st.MediaTitle, wantYear, wantSeason)

				curTrack.mu.Lock()
				curTrack.Active = true
				curTrack.SeriesKey = seriesKey
				curTrack.ParsedTitle = title
				curTrack.MediaTitle = st.MediaTitle
				curTrack.WantYear = wantYear
				curTrack.WantSeason = wantSeason
				curTrack.LastFileKey = key
				curTrack.mu.Unlock()
				setCorrectionMenuActive(seriesKey != "")

				var (
					aname, cover  string
					aid, totalEps int
					aerr          error
				)
				maxWait := 60 * time.Second
				wait := 5 * time.Second
				totalWait := 0 * time.Second
				attempt := 1

				for {
					qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
					if seriesKey != "" {
						if mid := overrides.Get(seriesKey); mid > 0 {
							m, err2 := getAniListMediaByID(qctx, mid, cfg.UserAgent)
							if err2 == nil {
								aname = mediaLiteTitle(m)
								cover = m.CoverImage.Large
								aid = m.ID
								totalEps = m.Episodes
								aerr = nil
							} else if errors.Is(err2, errAniListRateLimited) {
								aerr = err2
							} else {
								aname, cover, aid, totalEps, aerr = findAniList(qctx, title, wantYear, wantSeason, cfg.UserAgent)
							}
						} else {
							aname, cover, aid, totalEps, aerr = findAniList(qctx, title, wantYear, wantSeason, cfg.UserAgent)
						}
					} else {
						aname, cover, aid, totalEps, aerr = findAniList(qctx, title, wantYear, wantSeason, cfg.UserAgent)
					}
					cancel()

					if aerr == nil {
						break
					}
					if !errors.Is(aerr, errAniListRateLimited) {
						break
					}
					if totalWait >= maxWait || ctx.Err() != nil {
						break
					}

					if wait > maxWait-totalWait {
						wait = maxWait - totalWait
					}
					traySetResolving(title, ep, wait, attempt)
					time.Sleep(wait)
					totalWait += wait
					attempt++
				}

				if aerr != nil {
					logAppend("AniList lookup failed: ", aerr.Error())
					aname = title
					cover = ""
					aid = 0
					totalEps = 0
				}

				if strings.TrimSpace(ep) == "" && totalEps == 1 {
					ep = "1"
				}

				cache.mu.Lock()
				cache.lastFile = key
				cache.lastAni = aname
				cache.lastEp = ep
				cache.lastCover = cover
				cache.lastAniID = aid
				cache.lastTotalEps = totalEps
				cache.lastRewatch = anilistIsRewatching(ctx, aid)
				cache.startEpoch = time.Now().Add(-time.Duration(st.TimePos) * time.Second)
				cache.mu.Unlock()

				curTrack.mu.Lock()
				curTrack.LastAniID = aid
				curTrack.LastTotalEps = totalEps
				curTrack.mu.Unlock()

				malID := 0
				malTotalEps := 0
				if strings.TrimSpace(cfg.MALClientID) != "" {
					var merr error
					qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
					if seriesKey != "" {
						if mid := malOverrides.Get(seriesKey); mid > 0 {
							m, err2 := getMALAnimeByID(qctx, mid, cfg.MALClientID, cfg.UserAgent)
							if err2 == nil {
								malID = m.ID
								malTotalEps = m.Episodes
							} else {
								_, _, malID, malTotalEps, merr = findMAL(qctx, title, wantYear, wantSeason, cfg.MALClientID, cfg.UserAgent)
							}
						} else {
							_, _, malID, malTotalEps, merr = findMAL(qctx, title, wantYear, wantSeason, cfg.MALClientID, cfg.UserAgent)
						}
					} else {
						_, _, malID, malTotalEps, merr = findMAL(qctx, title, wantYear, wantSeason, cfg.MALClientID, cfg.UserAgent)
					}
					cancel()
					if merr != nil {
						logAppend("MAL lookup failed: ", merr.Error())
						malID = 0
						malTotalEps = 0
					}
				}

				curTrack.mu.Lock()
				curTrack.LastMALAnimeID = malID
				curTrack.LastMALTotalEps = malTotalEps
				curTrack.mu.Unlock()

				traySetWatching(aname, ep, -1, totalEps)
				reported = make(map[string]bool)
				aniSynced = make(map[string]bool)
				malSynced = make(map[string]bool)
				nextSyncAttempt = make(map[string]time.Time)

				if storeToggle("fillerWarn") && aid > 0 && strings.TrimSpace(ep) != "" {
					if epNum, err := strconv.Atoi(ep); err == nil && epNum > 0 {
						go func(aid int, aname string, epNum int, filenameTitle, parsedTitle, mediaTitle string) {
							if !markFillerWarned(aid, epNum) {
								return
							}

							titles := []string{
								aname,
								strings.TrimSpace(parsedTitle),
								strings.TrimSpace(filenameTitle),
								strings.TrimSpace(mediaTitle),
							}

							c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
							defer cancel()

							fi, err := getFillerInfo(c, aid, titles)
							if err != nil {
								logAppend("filler: lookup failed: ", err.Error())
								return
							}
							if fi.FillerEps[epNum] {
								msg := fmt.Sprintf("Você está assistindo a um episódio filler:\n\n%s — episódio %d\n\n(Fonte: animefillerlist.com)", aname, epNum)
								messageBox("Kouki: episódio filler", msg, mbOK|mbIconInfo)
							}
						}(aid, aname, epNum, key, title, st.MediaTitle)
					}
				}

				pushEstimated()
				broadcastHostState(currentFileKey)
				continue
			}

			if ready && !st.Pause && st.Duration > 0 {
				ratio := st.TimePos / st.Duration
				if ratio >= 0.80 {
					cache.mu.Lock()
					aid := cache.lastAniID
					epStr := cache.lastEp
					totalEps := cache.lastTotalEps
					cache.mu.Unlock()
					if epStr != "" {
						if epNum, err := strconv.Atoi(epStr); err == nil && epNum > 0 {
							if aid > 0 {
								k := fmt.Sprintf("%d:%d", aid, epNum)
								if !reported[k] {
									reported[k] = true
									simulBroadcastSync(simulSyncEvent{AniID: aid, Episode: epNum, TotalEps: totalEps, AtUnixSec: time.Now().Unix()})
								}

								if anilistTrackingEnabled() && !aniSynced[k] && !time.Now().Before(nextSyncAttempt["ani:"+k]) {
								store.mu.RLock()
								tok := store.AccessToken
								uid := store.UserID
								store.mu.RUnlock()
								c, cancel := context.WithTimeout(ctx, 8*time.Second)
								err := syncAniListProgress(c, tok, uid, aid, epNum, totalEps)
								cancel()
								if err != nil {
									logAppend("AniList sync error: ", err.Error())
									nextSyncAttempt["ani:"+k] = time.Now().Add(time.Minute)
								} else {
									aniSynced[k] = true
								}
							}
							}

							curTrack.mu.RLock()
							malID := curTrack.LastMALAnimeID
							malTotal := curTrack.LastMALTotalEps
							curTrack.mu.RUnlock()
							if malTrackingEnabled() && malID > 0 {
								k := fmt.Sprintf("%d:%d", malID, epNum)
								if !malSynced[k] && !time.Now().Before(nextSyncAttempt["mal:"+k]) {
									c, cancel := context.WithTimeout(ctx, 8*time.Second)
									err := syncMALProgress(c, cfg, malID, epNum, malTotal)
									cancel()
									if err != nil {
										logAppend("MAL sync error: ", err.Error())
										nextSyncAttempt["mal:"+k] = time.Now().Add(time.Minute)
									} else {
										malSynced[k] = true
									}
								}
							}
						}
					}
				}
			}

			pushEstimated()
			broadcastHostState(currentFileKey)

		case <-uiTicker.C:
			if joined() {
				simulApplyJoinStateToUI(cfg, mgr)
				continue
			}
			if playing && cfg.PollInterval > time.Second && (!lastMpvUpdateAt.IsZero()) {
				pushEstimated()
				broadcastHostState(currentFileKey)
			}
		}
	}
}
