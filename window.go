package main

// window.go — the native main window.
//
// Kouki used to be tray-only. This adds a real Windows window that exposes every
// action the notification-area menu offers, while the tray keeps working as the
// quick-access surface.
//
// Close/minimise semantics (required behaviour):
//   - closing the window hides it and Kouki keeps running in the tray
//   - minimising does the same
//   - "Abrir o Kouki" in the tray brings it back
//   - only the Quit button, or "Sair" in the tray, ends the process
//
// Toolkit choice
// --------------
// github.com/lxn/walk, a pure-Go (no cgo) Win32 UI library. It produces a real,
// OS-native window with a real message loop, which is what "a normal desktop app
// window" means on Windows — unlike a WebView/WebView2 surface, which draws HTML
// inside the app's window.
//
// The window's content is a single walk.CustomWidget that paints itself; see
// ui.go for why walk's stock controls cannot express the design and how the
// layout, hit-testing and repaint cycle work.
//
// Threading
// ---------
// walk owns the MAIN thread's message loop. The systray icon runs on a separate
// goroutine whose OS thread is locked, so the two message loops never contend.
// Anything the tray needs to do to a widget goes through Synchronize(), which walk
// documents as callable from any thread. refreshGuiSafe() is that bridge; the
// plain refreshGui() must only ever run on walk's thread, because Synchronize
// from there would deadlock.
//
// Layout
// ------
// A layout is MANDATORY on the form, and that is not a style choice:
//
//   - walk.ContainerBase.CreateLayoutItem dereferences the container's layout
//     field with no nil check: `return cb.layout.CreateLayoutItem(ctx)`.
//   - walk does NOT install a default one. FormBase.init creates the form's
//     clientComposite through NewComposite, and neither NewComposite nor the
//     library ever calls SetLayout on it.
//   - children are accidentally protected: createLayoutItemForWidgetWithContext
//     returns nil early when container.Layout() == nil, so they are merely
//     skipped. The ROOT container is not protected.
//   - FormBase.startLayout begins with CreateLayoutItemsForContainer
//     (fb.clientComposite), which for a Widget container calls
//     widget.CreateLayoutItem(ctx) and hits the nil dereference above.
//
// So a form shown without SetLayout panics before its first paint. Here the form
// layout is a zero-margin box holding exactly one child — the canvas — whose
// greedy layout item makes it fill the client area. Everything inside is computed
// in ui.go from pixel arithmetic instead.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/lxn/walk"
)

// ---------------------------------------------------------------------------
// window state shared with the rest of the app
// ---------------------------------------------------------------------------

// guiPlayer is the mpv process Kouki launched itself.
var guiPlayer = &mpvPlayer{}

// gui is the live window. Nil when no window could be created.
var gui struct {
	mu sync.Mutex
	w  *walk.MainWindow
}

// guiCanvas is the window's single drawn surface. Nil until runMainWindow
// builds it, and every repaint path tolerates that.
var guiCanvas *uiCanvas

// quitSignal is closed when Kouki must terminate.
var quitSignal = make(chan struct{})

var quitOnce sync.Once

// guiIsQuitting is set while the process is shutting down, so the Closing
// handler lets the window actually close instead of hiding it.
var guiIsQuitting bool

// guiFoundMpv holds the candidates found by the last detection run, so
// "Usar MPV encontrado" can pick from the same list the dialog showed.
var guiFoundMpv []mpvCandidate

// mpvScanOnce makes sure the automatic startup check runs only one time.
var mpvScanOnce sync.Once

// malCredentialsHint explains how to supply the MyAnimeList OAuth credentials.
const malCredentialsHint = "ID/segredo do cliente MAL ausente.\n\n" +
	"O login do MyAnimeList usa authorization-code e exige um ID e um SEGREDO " +
	"que so o dono do aplicativo MAL possui. O Kouki nao os distribui.\n\n" +
	"Registre seu proprio app em https://myanimelist.net/apiconfig " +
	"e defina:\n" +
	"    setx MAL_CLIENT_ID \"<seu id>\"\n" +
	"    setx MAL_CLIENT_SECRET \"<seu segredo>\"\n\n" +
	"Depois reinicie o terminal e o Kouki."

// guiError shows a message box, marshalling onto walk's thread when called from
// a background goroutine. walk widgets and modal dialogs must not be touched off
// the walk thread.
func guiError(title, text string) {
	gui.mu.Lock()
	mw := gui.w
	gui.mu.Unlock()
	if mw == nil {
		messageBox(title, text, mbOK|mbIconError)
		return
	}
	mw.Synchronize(func() { messageBox(title, text, mbOK|mbIconError) })
}

// ---------------------------------------------------------------------------
// construction and lifetime
// ---------------------------------------------------------------------------

// Default window size. The height is not a constant: it is measured from the
// layout (see uiCanvas.measure) and then clamped to the screen, so the content
// fills the window instead of leaving a dead band underneath it. Nothing is
// stretched to fill the space — the window shrinks to the content instead.
const (
	uiDefaultW = 1080
	uiMaxH     = 1000
	uiMinW     = 820
	uiMinH     = 520
)

// buildWindowUI installs the layout, the canvas and the window geometry.
//
// It is a separate function with its own recover for one reason: main() has no
// recover, and a -H=windowsgui build has no console. A panic while the window is
// being assembled unwinds the main goroutine and the process disappears with no
// window, no tray and no message — which is exactly how a layout bug presents
// itself. Here it becomes a logged error and a dialog instead.
func buildWindowUI(mw *walk.MainWindow, wa walk.Rectangle) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logRecoveredPanic("buildWindowUI")
			err = fmt.Errorf("%v", r)
		}
	}()

	winW := uiMinInt(uiDefaultW, wa.Width-40)
	if winW < 640 {
		winW = 640
	}

	// A layout is MANDATORY on the form: see the layout note at the top.
	formLayout := walk.NewVBoxLayout()
	_ = formLayout.SetMargins(walk.Margins{})
	_ = formLayout.SetSpacing(0)
	_ = mw.SetLayout(formLayout)

	canvas := newUICanvas(mw)
	if canvas == nil {
		return errors.New("não foi possível criar a superfície de desenho")
	}
	guiCanvas = canvas

	// Size the window to what the layout actually needs. A provisional size comes
	// first because measure() runs the same arithmetic the first paint will.
	_ = mw.SetClientSize(walk.Size{Width: winW, Height: uiMinH})
	winH := uiMinInt(uiMaxInt(uiMinH, canvas.measure(winW)), uiMinInt(uiMaxH, wa.Height-40))
	_ = mw.SetClientSize(walk.Size{Width: winW, Height: winH})

	_ = mw.SetMinMaxSize(
		walk.Size{
			Width:  uiMinInt(uiMinW, wa.Width-40),
			Height: uiMinInt(uiMinH, wa.Height-40),
		},
		walk.Size{Width: 32767, Height: 32767})
	return nil
}

// runMainWindow builds and shows the window, then blocks until Kouki quits.
//
// Must be called on the main thread: walk installs its message loop there.
func runMainWindow() {
	mw, err := walk.NewMainWindow()
	if err != nil {
		// Kouki is still fully usable from the tray, so degrade rather than fail.
		messageBox(appName,
			"Não foi possível criar a janela principal:\n\n"+err.Error()+
				"\n\nO "+appName+" continua disponível pela bandeja do sistema.",
			mbOK|mbIconWarning)
		waitForQuit()
		return
	}

	_ = mw.SetTitle(appName + " " + appVersion)

	// walk registers its window class with a NULL hIcon, which leaves the title
	// bar and the taskbar on the system default rather than on the executable's
	// own mark. Setting it explicitly from the embedded artwork also means the
	// title bar cannot drift away from the header, which paints the same source.
	if ic := uiWindowIcon(); ic != nil {
		if err := mw.SetIcon(ic); err != nil {
			logAppend("janela: icone nao aplicado: ", err.Error())
		}
	}

	// Clamp to the usable desktop area: on a short screen the window must still
	// fit, and the canvas scrolls for the rest.
	wa := workArea()

	if err := buildWindowUI(mw, wa); err != nil {
		_ = mw.Close()
		messageBox(appName,
			"Não foi possível montar a janela principal:\n\n"+err.Error()+
				"\n\nO detalhe está em "+filepath.Join(ConfigDir(), "kouki.log")+
				".\n\nO "+appName+" continua disponível pela bandeja do sistema.",
			mbOK|mbIconError)
		waitForQuit()
		return
	}

	gui.mu.Lock()
	gui.w = mw
	gui.mu.Unlock()

	// The window never closes on its own: X and minimise both hide it and leave
	// Kouki in the tray. Only requestQuit() sets guiIsQuitting and lets it go.
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if guiIsQuitting {
			return
		}
		*canceled = true
		mw.Hide()
	})

	// Keys and the wheel arrive on the form, not on the canvas: walk publishes
	// them to whichever window holds focus, and neither of these is focusable.
	// guiCanvas is the canvas buildWindowUI just installed; it cannot be nil
	// here, because that function returns an error instead of returning early.
	canvas := guiCanvas
	mw.KeyDown().Attach(func(key walk.Key) { canvas.OnKey(key) })
	mw.MouseWheel().Attach(canvas.onMouseWheel)

	refreshGui()
	// Opt into Windows' immersive dark mode before the first paint, so the title
	// bar, the client surface and the drawn canvas agree on one colour.
	applyDarkChrome(mw)
	mw.Show()

	// Detect / install / configure the player as soon as the window is up.
	guiScanMpvOnStartup()

	// Pump playback information while the process lives. walk widgets may only be
	// touched from walk's thread, hence Synchronize.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(750 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-quitSignal:
				return
			case <-stop:
				return
			case <-t.C:
				mw.Synchronize(refreshGui)
			}
		}
	}()

	mw.Run()
	close(stop)
}

// waitForQuit blocks until Kouki is asked to quit, for when no window exists.
func waitForQuit() {
	<-quitSignal
}

// showMainWindow brings the window back from the tray.
func showMainWindow() {
	gui.mu.Lock()
	mw := gui.w
	gui.mu.Unlock()
	if mw == nil {
		return
	}
	// Synchronize marshals onto walk's thread; Show is not safe from the tray's.
	mw.Synchronize(func() {
		mw.Show()
	})
}

// hideMainWindow sends the window back to the tray. Bound to Esc.
func hideMainWindow() {
	gui.mu.Lock()
	mw := gui.w
	gui.mu.Unlock()
	if mw == nil {
		return
	}
	mw.Synchronize(func() { mw.Hide() })
}

// requestQuit ends the process for real: stop the player we own, tear down
// Simulwatching, release the tray icon and close the window.
func requestQuit() {
	quitOnce.Do(func() {
		guiIsQuitting = true
		guiPlayer.Stop(guiPlayer.PipePath(), 3*time.Second)
		simulStop()
		systray.Quit()

		gui.mu.Lock()
		mw := gui.w
		gui.mu.Unlock()
		if mw != nil {
			mw.Synchronize(func() { _ = mw.Close() })
		}
		close(quitSignal)
	})
}

// ---------------------------------------------------------------------------
// repainting
// ---------------------------------------------------------------------------

// refreshGui asks for a repaint when the window would actually look different.
// The canvas re-reads every piece of state while it paints, so there is nothing
// to synchronise here beyond deciding *whether* to paint. Walk thread only.
func refreshGui() {
	if guiCanvas != nil {
		guiCanvas.InvalidateIfChanged()
	}
}

// refreshGuiSafe is refreshGui for background goroutines.
func refreshGuiSafe() {
	gui.mu.Lock()
	mw := gui.w
	gui.mu.Unlock()
	if mw == nil || guiCanvas == nil {
		return
	}
	mw.Synchronize(refreshGui)
}

// ---------------------------------------------------------------------------
// GUI actions
// ---------------------------------------------------------------------------

// guiAction runs a named action. Called from widget callbacks (walk's thread).
func guiAction(name string) {
	cfg := loadConfig()
	switch name {
	case "ani-login":
		go func() {
			if err := oauthLogin(context.Background()); err != nil {
				guiError("Kouki: AniList", err.Error())
			}
		}()
	case "ani-logout":
		store.Clear()
		refreshAuthMenu()
	case "mal-login":
		// The tray performs this same guard before starting; without it here the
		// button looked completely dead whenever MAL credentials are missing.
		if strings.TrimSpace(cfg.MALClientID) == "" || strings.TrimSpace(cfg.MALClientSecret) == "" {
			messageBox("Kouki: MyAnimeList", malCredentialsHint, mbOK|mbIconError)
			return
		}
		go func() {
			if err := malOAuthLogin(context.Background(), cfg); err != nil {
				guiError("Kouki: MyAnimeList", err.Error())
			}
		}()
	case "mal-logout":
		store.ClearMAL()
		refreshAuthMenu()

	case "ani-correct", "mal-correct":
		curTrack.mu.RLock()
		active := curTrack.Active && curTrack.LastFileKey != ""
		seriesKey, query := curTrack.SeriesKey, curTrack.ParsedTitle
		curTrack.mu.RUnlock()
		if !active || seriesKey == "" || query == "" {
			messageBox(appName, "Nenhum arquivo em reprodução.", mbOK|mbIconInfo)
			return
		}
		if name == "ani-correct" {
			_ = startAniListSelector(query, cfg.UserAgent, func(id int) {
				overrides.Set(seriesKey, id)
				select {
				case resolveCh <- resolveRequest{SeriesKey: seriesKey, MediaID: id}:
				default:
				}
			})
			return
		}
		_ = startMALSelector(query, cfg.MALClientID, cfg.UserAgent, func(id int) {
			malOverrides.Set(seriesKey, id)
			select {
			case resolveMALCh <- resolveMALRequest{SeriesKey: seriesKey, AnimeID: id}:
			default:
			}
		})

	case "simul-host":
		// simulStartHost takes no code: the shared runtime reads it from
		// simul.code, so the code chosen in the UI must be stored there first.
		// This mirrors the tray handler in main.go exactly.
		go simulStartHostUI(context.Background(), func(code string) {
			simul.mu.Lock()
			simul.code = code
			simul.mu.Unlock()
			go simulStartHost(context.Background())
		})
	case "simul-join":
		go simulStartJoinUI(context.Background(), cfg.UserAgent, func(addr, code string) {
			go simulStartJoin(context.Background(), addr, code)
		})
	case "simul-stop":
		simulStop()

	case "mpv-launch":
		exe := resolveMpvPath()
		if exe == "" {
			guiNoMpv()
			return
		}
		if err := guiPlayer.Launch(exe, mpvLaunchOptions{Pipe: pipeLeaf(mpvPipeName())}); err != nil {
			messageBox(appName, "Não foi possível iniciar o MPV:\n\n"+err.Error(), mbOK|mbIconError)
			return
		}
		go watchMpvStart(exe)
	case "mpv-stop":
		// The player may have been opened by hand, in which case guiPlayer
		// never tracked it and PipePath() is empty. The live connection Kouki
		// holds (or a fresh dial on the configured pipe) is what actually
		// reaches it; use that instead of relying on the tracked process.
		if conn := sharedMpvConn.get(); conn != nil {
			_ = conn.setCommand("quit")
			_ = conn.Close()
			sharedMpvConn.set(nil)
		} else {
			pipe := guiPlayer.PipePath()
			if pipe == "" {
				pipe = pipeLeaf(mpvPipeName())
			}
			guiPlayer.Stop(pipe, 3*time.Second)
			sharedMpvConn.set(nil)
		}

	case "mpv-detect":
		guiDetectMpv()
	case "mpv-use":
		guiUseFoundMpv()
	case "mpv-install":
		guiInstallMpv()

	case "mpv-toggle-pause", "mpv-play", "mpv-pause", "mpv-next", "mpv-previous",
		"mpv-fullscreen", "mpv-volume-up", "mpv-volume-down":
		if err := mpvDispatch(sharedMpvConn.get(), strings.TrimPrefix(name, "mpv-")); err != nil {
			messageBox(appName, err.Error(), mbOK|mbIconInfo)
		}

	case "open-install-folder":
		openInstallFolder()
	case "open-config-folder":
		_ = os.MkdirAll(ConfigDir(), 0o700)
		openFolder(ConfigDir())
	case "open-mpv-folder":
		p := resolveMpvPath()
		if p == "" {
			guiNoMpv()
			return
		}
		openFolder(filepath.Dir(p))

	case "reinstall":
		if err := runSetupExe(); err != nil {
			messageBox(appName,
				"Não foi possível abrir o instalador:\n\n"+err.Error()+
					"\n\nVocê também pode usar Configurações > Aplicativos > Kouki.",
				mbOK|mbIconWarning)
		}

	case "about":
		showAboutDialog()
	case "quit":
		requestQuit()
	}

	refreshGui()
}

// storeSaveQueued and storeSaveMu drive the deferred write below.
var (
	storeSaveMu     sync.Mutex
	storeSaveQueued bool
)

// storeSaveAsync persists the store without blocking walk's thread.
//
// authStore.Save is a synchronous os.WriteFile. Called straight from a widget
// callback it blocks the UI thread on disk I/O, and it does so *before* the
// repaint the click asked for — which is how a 40x22 toggle turns into a
// hundred-millisecond freeze followed by a redraw that lands late enough to
// read as a flicker.
//
// The loop collapses a burst of clicks into the writes it actually needs, and
// each pass re-reads the store under its own lock, so the last write always
// carries the final values.
func storeSaveAsync() {
	storeSaveMu.Lock()
	storeSaveQueued = true
	storeSaveMu.Unlock()

	go func() {
		storeSaveMu.Lock()
		for storeSaveQueued {
			storeSaveQueued = false
			storeSaveMu.Unlock()
			store.Save()
			storeSaveMu.Lock()
		}
		storeSaveMu.Unlock()
	}()
}

// guiSetToggle applies and persists a boolean setting.
func guiSetToggle(key string, value bool) {
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
		return
	}
	store.mu.Unlock()

	storeSaveAsync()
	refreshAuthMenu()

	// A switch click repaints only that switch: nothing else on screen depends on
	// it, and dirtying the whole surface for a 40x22 change is what used to make
	// every toggle feel heavier than it looked.
	if guiCanvas != nil {
		guiCanvas.InvalidateSwitch(key)
	}

	if key == "runOnStartup" {
		if err := setWindowsRunOnStartup(value); err != nil {
			messageBox(appName,
				"Não foi possível alterar a inicialização automática:\n\n"+err.Error(),
				mbOK|mbIconError)
		}
	}
}

// ---------------------------------------------------------------------------
// MPV discovery
// ---------------------------------------------------------------------------

// setSelectedMpvUI records the player's choice and drops the memoised MPV line,
// so the Reprodução card picks up the new executable on its next repaint.
func setSelectedMpvUI(path string) {
	setSelectedMpv(path)
	invalidateUIMpvDisplay()
}

// guiScanMpvOnStartup is the behaviour asked for when Kouki opens:
//
//   - is an MPV already installed (the build people download from GitHub)?
//   - if not, install the MPV that ships inside Kouki, already configured
//   - if yes, only fix its mpv.conf so the IPC channel opens
//
// It stays silent when everything is already fine and speaks up only when it had
// to install something or hit a problem.
func guiScanMpvOnStartup() {
	mpvScanOnce.Do(func() {
		go func() {
			// Let the window finish its first layout pass so the dialogs below
			// have a real parent to attach to.
			time.Sleep(600 * time.Millisecond)

			existing := FindMpvCandidates()
			sortCandidates(existing)
			logAppend("mpv: varredura encontrou ", fmt.Sprint(len(existing)))
			for _, c := range existing {
				logAppend("mpv: candidato ", c.Path)
			}

			if len(existing) > 0 {
				guiFoundMpv = existing

				chosen := existing[0]
				setSelectedMpvUI(chosen.Path)
				if err := EnsureMpvIpcConfig(chosen.Path); err != nil {
					refreshGuiSafe()
					guiError(appName, "MPV encontrado em\n"+chosen.Path+
						"\n\nMas o mpv.conf não pôde ser ajustado:\n"+err.Error())
					return
				}
				refreshGuiSafe()
				return
			}

			// Nothing installed: lay down the bundled copy for this user.
			exe, installed, err := InstallAndConfigureMpv(BundledMpvInstallDir())
			if err != nil {
				guiError(appName,
					"Nenhum MPV encontrado e a instalação automática falhou.\n\n"+err.Error()+
						"\n\nUse “Instalar MPV” na aba Reprodução para tentar de novo.")
				return
			}

			setSelectedMpvUI(exe)
			refreshGuiSafe()
			logAppend("mpv: instalado em ", exe, " (novo=", fmt.Sprint(installed), ")")

			if installed {
				guiError(appName,
					"Nenhum MPV estava instalado.\n\n"+
						"O MPV que acompanha o "+appName+" foi instalado em:\n"+exe+
						"\n\nEle já está com o canal IPC configurado, então o Kouki"+
						" acompanha a reprodução assim que você começar a assistir.")
			}
		}()
	})
}

// watchMpvStart waits for the launched player to expose its IPC channel and
// reports what actually happened if it never does.
//
// This used to sleep a fixed three seconds and then ask whether the GUI had a
// state snapshot. That check raced the poll loop, whose cycle while no player is
// running is dial (up to 2 s) plus sleep — so the dialog fired while the channel
// was still about to be opened and reported a failure that was not one. It also
// pointed at %APPDATA%\Kouki\mpv.conf, which mpv never reads.
//
// The poll loop owns the connection — mpv serves one client at a time, so
// opening a second pipe here would fight it — so this only observes. The player
// is never stopped on the strength of a failed check.
func watchMpvStart(exe string) {
	const (
		poll    = 250 * time.Millisecond
		giveUp  = 12 * time.Second
		settled = 2 * time.Second
	)

	start := time.Now()
	var firstFailure string

	for {
		if sharedMpvConn.get() != nil {
			logAppend("mpv: conectado em ", time.Since(start).Round(time.Millisecond).String())
			return
		}
		if !guiPlayer.IsRunning() {
			// reap already recorded the exit code and what the player wrote.
			logAppend("mpv: encerrou antes de abrir o canal IPC")
			guiError(appName,
				"O MPV iniciou e em seguida encerrou.\n\n"+
					"O log do player explica o motivo:\n"+
					filepath.Join(ConfigDir(), "mpv.log"))
			return
		}
		if e := sharedMpvConn.LastError(); e != "" && firstFailure == "" {
			firstFailure = e
		}

		// A player that has been up for a while and still has no channel is a
		// real failure; the extra grace covers mpv taking its time on first run.
		if time.Since(start) > giveUp && time.Since(start) > settled {
			break
		}
		time.Sleep(poll)
	}

	// Report the truth: the channel did not appear, here is the last reason we
	// have, and here is the file mpv actually reads.
	cfgPath := mpvConfigPathFor(exe)
	if cfgPath == "" {
		cfgPath = filepath.Join(filepath.Dir(exe), "portable_config", "mpv.conf")
	}

	detail := ""
	if firstFailure != "" {
		detail = "\n\nÚltima tentativa de conexão:\n" + firstFailure
	}

	logAppend("mpv: canal IPC não apareceu em ", time.Since(start).Round(time.Millisecond).String(),
		"; ultima falha: ", firstFailure)

	guiError(appName,
		"O MPV está aberto, mas o canal IPC não apareceu.\n\n"+
			"O arquivo que o mpv lê é:\n"+cfgPath+detail+
			"\n\nO player continua aberto: você pode usá-lo normalmente. "+
			"Os controles do Kouki só funcionam quando o canal responde.")
}

// guiNoMpv explains that nothing was found anywhere and offers the installer.
func guiNoMpv() {
	messageBox(appName,
		"Nenhum MPV encontrado.\n\n"+
			"Pesquise os locais padrão (Reprodução → Procurar MPV). O MPV pode ser "+
			"instalado automaticamente pelo Kouki quando não for detectado, ou pelo "+
			"instalador do "+appName+".",
		mbOK|mbIconWarning)
}

// guiDetectMpv scans the standard folders and the PATH, then reports what it
// found. It is read-only.
func guiDetectMpv() {
	list := FindMpvCandidates()
	sortCandidates(list)
	guiFoundMpv = list

	// The MPV may have been installed or removed since the card last repainted,
	// so the memoised line has to be resolved again.
	invalidateUIMpvDisplay()
	refreshGui()

	if len(list) == 0 {
		messageBox(appName,
			"Nenhum mpv.exe encontrado.\n\n"+
				"Pastas varridas:\n"+
				"  • o diretório do "+appName+"\n"+
				"  • %USERPROFILE%\\Documents\\mpv e Downloads\\mpv\n"+
				"  • %LOCALAPPDATA%\\Programs\\mpv\n"+
				"  • %ProgramFiles%\\mpv\n"+
				"  • scoop, chocolatey e o PATH\n\n"+
				"Use “Instalar MPV” para instalar o MPV que vem com o projeto.",
			mbOK|mbIconWarning)
		return
	}

	var b strings.Builder
	for _, c := range list {
		mark := "  "
		if samePath(c.Path, FindBundledMpv()) {
			mark = "✓ "
		}
		b.WriteString(mark + c.Path + "\n")
		if c.Version != "" {
			b.WriteString("    " + c.Version + "\n")
		}
		if c.Config != "" {
			b.WriteString("    config: " + c.Config + "\n")
		}
	}

	messageBox(appName+" — MPV encontrado",
		b.String()+"\nO primeiro da lista pode ser adotado pelo menu de contexto.",
		mbOK|mbIconInfo)
}

// guiUseFoundMpv adopts the first candidate from the last detection run and makes
// sure its config exposes the IPC pipe.
func guiUseFoundMpv() {
	if len(guiFoundMpv) == 0 {
		guiDetectMpv()
		if len(guiFoundMpv) == 0 {
			return
		}
	}

	chosen := guiFoundMpv[0]
	setSelectedMpvUI(chosen.Path)

	note := ""
	if err := EnsureMpvIpcConfig(chosen.Path); err != nil {
		note = "\n\nNão foi possível ajustar o mpv.conf: " + err.Error()
	} else {
		note = "\n\nCanal IPC configurado - o Kouki acompanha a reprodução."
	}

	refreshGui()

	messageBox(appName,
		"MPV selecionado:\n"+describeMpvPath(chosen.Path)+note,
		mbOK|mbIconInfo)
}

// guiInstallMpv installs the MPV that ships with the project.
//
// There is no payload to extract at runtime — the ~127 MB tree lives in the
// Inno Setup package — so this relaunches the setup with the MPV component,
// and then verifies the IPC line.
func guiInstallMpv() {
	// Already present? Then there is nothing to install — just guarantee IPC.
	if exe := FindBundledMpv(); exe != "" {
		if err := EnsureMpvIpcConfig(exe); err != nil {
			messageBox(appName, "MPV já está instalado, mas o mpv.conf não pôde ser ajustado:\n\n"+err.Error(),
				mbOK|mbIconWarning)
			return
		}
		// Make sure a player that is present is also registered, in case it was
		// dropped here by an older build that did not do this.
		registerMpvForWindows(exe)
		invalidateUIMpvDisplay()
		refreshGui()
		messageBox(appName,
			"O MPV já estava instalado em:\n"+exe+
				"\n\nO canal IPC foi confirmado, então o Kouki acompanha a reprodução.",
			mbOK|mbIconInfo)
		return
	}

	// Not installed: the copy lives inside the setup package, so run it.
	if err := runSetupExe(); err != nil {
		messageBox(appName,
			"O MPV ainda não foi instalado neste computador.\n\n"+
				"O MPV vem dentro do instalador ("+setupExeName+"), que não foi encontrado aqui.\n"+
				"Rode o instalador de novo e mantenha o componente MPV marcado.\n\n"+
				"Erro: "+err.Error(),
			mbOK|mbIconWarning)
		return
	}

	messageBox(appName,
		"O instalador foi aberto.\n\n"+
			"Mantenha o componente MPV marcado e conclua. Ao terminar, use "+
			"“Procurar MPV” para confirmar que o canal IPC foi configurado.",
		mbOK|mbIconInfo)
}

// formatDuration renders seconds as h:mm:ss or m:ss.
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
