package main

// install.go — installation layout, configuration locations and the small
// Windows integrations Kouki needs at runtime.
//
// Installation itself is handled by Inno Setup (see installer/kouki.iss). This
// file only describes the layout that installer produces and resolves those
// paths at runtime, so the application keeps working from any install directory.
//
// Layout produced by the installer:
//
//	%LOCALAPPDATA%\Programs\Kouki\Kouki.exe
//	%LOCALAPPDATA%\Programs\Kouki\kouki.ico
//	%LOCALAPPDATA%\Programs\Kouki\mpv\...           (optional component)
//	%APPDATA%\Kouki\auth.json                        (settings + tokens)
//
// Nothing here requires administrator rights.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ---------------------------------------------------------------------------
// identity
// ---------------------------------------------------------------------------

const (
	appName    = "Kouki"
	appVersion = "1.0.1"
	// appTagline is the one-line description under the wordmark in the header.
	// It describes what the application does, not what it integrates with: a
	// tagline naming a third party goes stale the moment that feature changes.
	appTagline   = "Contas, reprodução e progresso"
	appExeName   = "Kouki.exe"
	iconFileName = "kouki.ico"

	// legacyName is the name of the previous release. It is kept only as the
	// source side of the one-way migration in MigrateLegacyConfig and of the
	// stale-autostart cleanup, both of which must target the OLD product.
	legacyName = "Koushin"

	mpvSubDir = "mpv"

	// setupExeName is the Inno Setup package, looked up next to the binary and in
	// the user's Downloads folder so "Reinstall" can relaunch it.
	setupExeName = "Kouki-1.0.1-Setup.exe"

	// appSingleInstPort is the loopback port used as the single-instance guard.
	appSingleInstPort = 45222
)

// ---------------------------------------------------------------------------
// paths — always resolved dynamically, never hardcoded
// ---------------------------------------------------------------------------

// localAppData is %LOCALAPPDATA% (machine-specific; where programs go for a
// per-user install).
func localAppData() string {
	if v := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); v != "" {
		return v
	}
	if h := strings.TrimSpace(os.Getenv("USERPROFILE")); h != "" {
		return filepath.Join(h, "AppData", "Local")
	}
	return os.TempDir()
}

// appData is %APPDATA% (roaming user configuration).
func appData() string {
	if v := strings.TrimSpace(os.Getenv("APPDATA")); v != "" {
		return v
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		if h := strings.TrimSpace(os.Getenv("USERPROFILE")); h != "" {
			return filepath.Join(h, "AppData", "Roaming")
		}
		return os.TempDir()
	}
	return base
}

// InstallRoot is %LOCALAPPDATA%\Programs\Kouki — the per-user install target.
func InstallRoot() string {
	return filepath.Join(localAppData(), "Programs", appName)
}

// ConfigDir is %APPDATA%\Kouki — user settings survive upgrades and reinstalls.
func ConfigDir() string {
	return filepath.Join(appData(), appName)
}

func bundledMpvDir(root string) string { return filepath.Join(root, mpvSubDir) }
func bundledMpvExe(root string) string { return filepath.Join(root, mpvSubDir, "mpv.exe") }

// ---------------------------------------------------------------------------
// bundled mpv discovery
// ---------------------------------------------------------------------------

// FindBundledMpv returns the bundled mpv.exe, or "" when MPV was not installed.
//
// It prefers a copy sitting next to the running binary so a user-chosen install
// directory keeps working, and falls back to the documented default location
// when the app is started from somewhere else.
func FindBundledMpv() string {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); fileExists(filepath.Join(dir, mpvSubDir, "mpv.exe")) {
			return filepath.Join(dir, mpvSubDir, "mpv.exe")
		}
	}
	if p := bundledMpvExe(InstallRoot()); fileExists(p) {
		return p
	}
	return ""
}

// findSetupExe locates the Inno Setup package so the GUI can offer "Reinstall".
func findSetupExe() string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, setupExeName),
			filepath.Join(filepath.Dir(dir), setupExeName),
		)
	}
	for _, dir := range []string{InstallRoot(), localAppData()} {
		candidates = append(candidates, filepath.Join(dir, setupExeName))
	}
	dl := strings.TrimSpace(os.Getenv("USERPROFILE"))
	if dl != "" {
		candidates = append(candidates, filepath.Join(dl, "Downloads", setupExeName))
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// legacy migration (Koushin -> Kouki)
// ---------------------------------------------------------------------------

// MigrateLegacyConfig copies %APPDATA%\Koushin into %APPDATA%\Kouki when the new
// location has no settings yet. The legacy folder is intentionally left in place
// so nothing is ever destroyed; it is safe to run on every start.
func MigrateLegacyConfig() error {
	old := filepath.Join(appData(), legacyName)
	if strings.EqualFold(old, ConfigDir()) {
		return nil
	}
	if !fileExists(old) {
		return nil
	}

	entries, err := os.ReadDir(old)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	dst := ConfigDir()
	copied := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(old, e.Name())
		target := filepath.Join(dst, e.Name())
		// Never clobber something the user already has in the new location.
		if fileExists(target) {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.WriteFile(target, data, 0o600); err == nil {
			copied++
		}
	}
	if copied > 0 {
		logAppend("install: migrou ", fmt.Sprintf("%d", copied), " arquivo(s) de ", old)
	}
	return nil
}

// removeLegacyRunKey deletes the autostart value written by the previous
// release, so a migrated install does not end up launching twice.
func removeLegacyRunKey() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(legacyName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// single instance
// ---------------------------------------------------------------------------

// appMutexName is the named mutex that marks a running instance. It is the same
// name the installer declares as AppMutex, which is what lets Setup notice a
// running Kouki and ask for it to be closed instead of failing on a locked
// Kouki.exe. Nothing created that mutex before, so that check never fired.
const appMutexName = "KoukiSingleInstanceMutex"

// acquireSingleInstanceGuard marks this process as the running instance for as
// long as it lives.
//
// The mutex is the authority. The loopback port is only a doorbell: a second
// copy connects to it so the first one brings its window forward. Before, the
// port was the guard itself, so any unrelated program holding 45222 made Kouki
// exit silently at every start, and launching Kouki again while it sat in the
// tray did nothing visible at all.
//
// Returns the release function, or an error when another instance is running.
func acquireSingleInstanceGuard() (func(), error) {
	var mutex windows.Handle
	if name, err := windows.UTF16PtrFromString(appMutexName); err == nil {
		h, merr := windows.CreateMutex(nil, false, name)
		switch {
		case merr == nil:
			mutex = h
		case errors.Is(merr, windows.ERROR_ALREADY_EXISTS):
			if h != 0 {
				_ = windows.CloseHandle(h)
			}
			notifyRunningInstance()
			return nil, errors.New("outra instância do " + appName + " já está em execução")
		}
		// Any other failure leaves mutex at zero and the port guard below as the
		// only protection, which is the previous behaviour.
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", appSingleInstPort))
	if err != nil {
		if mutex == 0 {
			return nil, errors.New("outra instância do " + appName + " já está em execução")
		}
		// We own the mutex, so we are the instance; the doorbell is just
		// unavailable because something else has the port.
		logAppend("single-instance: porta ", fmt.Sprint(appSingleInstPort), " indisponível: ", err.Error())
		return func() { _ = windows.CloseHandle(mutex) }, nil
	}

	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
			showMainWindow()
		}
	}()

	return func() {
		_ = ln.Close()
		if mutex != 0 {
			_ = windows.CloseHandle(mutex)
		}
	}, nil
}

// notifyRunningInstance rings the doorbell of the instance that is already up.
func notifyRunningInstance() {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", appSingleInstPort), 500*time.Millisecond)
	if err == nil {
		_ = c.Close()
	}
}

// ---------------------------------------------------------------------------
// desktop integration
// ---------------------------------------------------------------------------

// openInstallFolder reveals the installed directory in Explorer.
func openInstallFolder() {
	dir := ""
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	if dir == "" || !fileExists(dir) {
		dir = InstallRoot()
	}
	if err := exec.Command("explorer.exe", dir).Start(); err != nil {
		messageBox(appName,
			"Não foi possível abrir a pasta:\n\n"+dir, mbOK|mbIconError)
	}
}

// openFolder reveals an arbitrary directory, reporting failures in the UI.
func openFolder(dir string) {
	if dir == "" || !fileExists(dir) {
		messageBox(appName, "Pasta não encontrada:\n\n"+dir, mbOK|mbIconWarning)
		return
	}
	if err := exec.Command("explorer.exe", dir).Start(); err != nil {
		messageBox(appName, "Não foi possível abrir a pasta:\n\n"+dir, mbOK|mbIconError)
	}
}

// runSetupExe relaunches the Inno Setup package so the user can repair,
// modify or remove the installation. Windows Installer also exposes those
// actions from "Apps & features".
func runSetupExe() error {
	setup := findSetupExe()
	if setup == "" {
		return errors.New("instalador (" + setupExeName + ") não encontrado")
	}
	cmd := exec.Command(setup)
	cmd.Dir = filepath.Dir(setup)
	return cmd.Start()
}

// showAboutDialog reports the version and the paths the app actually resolved at
// runtime, which is the first thing to ask for in a bug report.
func showAboutDialog() {
	mpvInfo := "não encontrado"
	if p := resolveMpvPath(); p != "" {
		if v := mpvVersionLine(p); v != "" {
			mpvInfo = v
		} else {
			mpvInfo = filepath.Base(p)
		}
	}

	messageBox("Sobre o "+appName,
		appName+" "+appVersion+"\n"+
			appTagline+"\n\n"+
			"Programa: "+installDirOrRunning()+"\n"+
			"Configurações: "+ConfigDir()+"\n"+
			"MPV: "+mpvInfo+"\n\n"+
			"Licença MIT.\n"+
			"O MPV é software de terceiros — veja THIRD-PARTY-NOTICES.md.",
		mbOK|mbIconInfo)
}

// installDirOrRunning is the directory the running binary lives in.
func installDirOrRunning() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return InstallRoot()
}

// mpvVersionLine asks the bundled mpv for its version banner.
func mpvVersionLine(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(line) > 90 {
		line = line[:90] + "…"
	}
	return line
}
