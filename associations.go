package main

// associations.go - Windows integration that goes beyond what the Inno Setup
// installer does on its own.
//
// Two goals:
//
//  1. Kouki itself should always be reachable from the Start Menu and from the
//     "Run" dialog, even when the user ran Kouki.exe directly instead of going
//     through the installer. The installer already creates the shortcut, but a
//     portable copy does not, and nothing downstream should depend on the user
//     having clicked through the wizard.
//
//  2. When Kouki installs MPV, that player should behave like a program the
//     user installed: registered under App Paths, present in "Open with", exposed
//     to the Default Programs control panel, on the Start Menu, and - the part the
//     user actually asked for - set as the default handler for video files.
//
// Everything here is per-user (HKCU) and needs no elevation, which matches the
// installer's PrivilegesRequired=lowest. Failures are logged, never fatal: a
// missing association must not stop Kouki from running.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// hiddenCmd builds an *exec.Cmd that never allocates a console window. Kouki is
// a GUI-subsystem binary, so any child that is a console application
// (PowerShell, mpv --version, ...) would otherwise flash a CMD window on
// startup or while the user browses the interface.
func hiddenCmd(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return c
}

// koukiStartMenuPath is where the per-user shortcut lives.
func koukiStartMenuPath() string {
	return filepath.Join(os.Getenv("APPDATA"),
		"Microsoft", "Windows", "Start Menu", "Programs", appName+".lnk")
}

// ensureKoukiStartMenu creates the Start Menu shortcut if it is missing.
// Idempotent: an existing shortcut is left alone so a user who pointed it
// somewhere else does not have it overwritten every launch.
func ensureKoukiStartMenu() {
	path := koukiStartMenuPath()
	if _, err := os.Stat(path); err == nil {
		return
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		// err is nil when only the path came back empty, so it cannot be
		// dereferenced unconditionally.
		reason := "caminho vazio"
		if err != nil {
			reason = err.Error()
		}
		logAppend("menu iniciar: exe nao resolvido: ", reason)
		return
	}
	if err := writeShortcut(path, exe, ""); err != nil {
		logAppend("menu iniciar: atalho do Kouki nao criado: ", err.Error())
		return
	}
	logAppend("menu iniciar: atalho do Kouki criado em ", path)
}

// ensureKoukiAppPaths registers the running executable under App Paths, so
// ShellExecute and the Run dialog can find it by name. Idempotent.
func ensureKoukiAppPaths() {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\App Paths\`+appExeName,
		registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		logAppend("app paths (Kouki): ", err.Error())
		return
	}
	defer key.Close()
	_ = key.SetStringValue("", exe)
	_ = key.SetDWordValue("UseUrl", 1)
}

// registerMpvForWindows integrates an mpv.exe that Kouki installed into
// Windows the way the reference mpv-install.bat does: App Paths, an Applications
// entry with play/open verbs, "Open with" lists for video and audio, a
// Capabilities key for the Default Programs control panel, the Start Menu
// shortcut, and - the part the user actually asked for - mpv set as the
// default handler for video files.
//
// It is per-user and needs no elevation. Errors are logged and never abort the
// install: a partial registration still leaves mpv usable from the command line.
func registerMpvForWindows(exe string) {
	exe = strings.TrimSpace(exe)
	if exe == "" || !fileExists(exe) {
		return
	}
	logAppend("mpv: registrando no Windows: ", exe)

	// --- App Paths ---------------------------------------------------------
	if key, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\App Paths\mpv.exe`,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("", exe)
		_ = key.SetDWordValue("UseUrl", 1)
		key.Close()
	} else {
		logAppend("mpv: App Paths: ", err.Error())
	}

	// --- Applications\mpv.exe ---------------------------------------------
	appKey := `Software\Classes\Applications\mpv.exe`
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, appKey,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("FriendlyAppName", "mpv")
		key.Close()
	}
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, appKey+`\shell`,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("", "play")
		key.Close()
	}
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, appKey+`\shell\open`,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("LegacyDisable", "")
		key.Close()
	}
	for _, sub := range []string{appKey + `\shell\open\command`, appKey + `\shell\play\command`} {
		if key, _, err := registry.CreateKey(registry.CURRENT_USER, sub,
			registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
			_ = key.SetStringValue("", fmt.Sprintf(`"%s" -- "%s"`, exe, "%L"))
			key.Close()
		}
	}
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, appKey+`\shell\play`,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("", "&Play")
		key.Close()
	}

	// --- Open with lists ---------------------------------------------------
	for _, kind := range []string{"video", "audio"} {
		path := `Software\Classes\SystemFileAssociations\` + kind + `\OpenWithList\mpv.exe`
		if key, _, err := registry.CreateKey(registry.CURRENT_USER, path,
			registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
			key.Close()
		}
	}

	// --- Capabilities (Default Programs) ----------------------------------
	capKey := `Software\Clients\Media\mpv\Capabilities`
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, capKey,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("ApplicationName", "mpv")
		_ = key.SetStringValue("ApplicationDescription", "mpv media player")
		key.Close()
	}

	// SupportedTypes + FileAssociations for the common extensions.
	exts := []string{
		// video
		".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".wmv", ".m4v",
		".mpg", ".mpeg", ".ts", ".m2ts", ".vob", ".ogv", ".3gp", ".3g2",
		".divx", ".rm", ".rmvb", ".asf", ".f4v",
		// audio
		".mp3", ".flac", ".wav", ".aac", ".ogg", ".m4a", ".wma", ".opus",
		".aiff", ".ape", ".wv", ".mka",
	}
	supportedKey := appKey + `\SupportedTypes`
	fileAssocKey := capKey + `\FileAssociations`
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, supportedKey,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		for _, ext := range exts {
			_ = key.SetStringValue(ext, "")
		}
		key.Close()
	}
	if key, _, err := registry.CreateKey(registry.CURRENT_USER, fileAssocKey,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		for _, ext := range exts {
			_ = key.SetStringValue(ext, "io.mpv"+ext)
		}
		key.Close()
	}

	// ProgIds (minimal: so the extension has something to resolve to even when
	// the user has never opened the Default Programs panel).
	for _, ext := range exts {
		progID := "io.mpv" + ext
		if key, _, err := registry.CreateKey(registry.CURRENT_USER,
			`Software\Classes\`+progID, registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
			_ = key.SetStringValue("", "mpv")
			key.Close()
		}
		if key, _, err := registry.CreateKey(registry.CURRENT_USER,
			`Software\Classes\`+progID+`\DefaultIcon`,
			registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
			_ = key.SetStringValue("", exe+",0")
			key.Close()
		}
		if key, _, err := registry.CreateKey(registry.CURRENT_USER,
			`Software\Classes\`+progID+`\shell\open\command`,
			registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
			_ = key.SetStringValue("", fmt.Sprintf(`"%s" -- "%s"`, exe, "%L"))
			key.Close()
		}
	}

	// --- RegisteredApplications -------------------------------------------
	if key, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\RegisteredApplications`,
		registry.SET_VALUE|registry.QUERY_VALUE); err == nil {
		_ = key.SetStringValue("mpv", "Software\\Clients\\Media\\mpv\\Capabilities")
		key.Close()
	}

	// --- Start Menu shortcut ----------------------------------------------
	sm := filepath.Join(os.Getenv("APPDATA"),
		"Microsoft", "Windows", "Start Menu", "Programs", "mpv.lnk")
	if _, err := os.Stat(sm); err != nil {
		if err := writeShortcut(sm, exe, ""); err != nil {
			logAppend("mpv: atalho do menu iniciar: ", err.Error())
		} else {
			logAppend("mpv: atalho do menu iniciar criado em ", sm)
		}
	}

	// --- make mpv the default for video ------------------------------------
	// IApplicationAssociationRegistration is the documented way to set defaults
	// without touching the protected UserChoice hash, which changes with every
	// Windows build and is not meant to be written by hand.
	setMpvAsDefaultViaCOM()
}

// writeShortcut creates a .lnk via the WScript.Shell COM object, the same
// approach the reference mpv-install.bat uses. It works without elevation.
func writeShortcut(lnkPath, target, args string) error {
	if err := os.MkdirAll(filepath.Dir(lnkPath), 0o700); err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		`$s=(New-Object -COM WScript.Shell).CreateShortcut('%s');$s.TargetPath='%s';$s.Arguments='%s';$s.Save()`,
		strings.ReplaceAll(lnkPath, "'", "''"),
		strings.ReplaceAll(target, "'", "''"),
		strings.ReplaceAll(args, "'", "''"))
	c := hiddenCmd("powershell", "-NoProfile", "-NonInteractive", "-Command", cmd)
	c.Stdout = nil
	c.Stderr = nil
	if err := c.Run(); err != nil {
		return fmt.Errorf("powershell: %w", err)
	}
	return nil
}

// setMpvAsDefaultViaCOM invokes IApplicationAssociationRegistration to set mpv
// as the default for every type its Capabilities claim. Failures are logged: on
// some Windows builds the COM object is restricted and the user has to pick mpv
// in Settings > Default apps, which the registration above makes possible.
func setMpvAsDefaultViaCOM() {
	ps := `$clsid=[Guid]::new('591209c7-767b-42b2-9fba-44ee4615f20c');` +
		`$t=[Type]::GetTypeFromCLSID($clsid);` +
		`$a=[Activator]::CreateInstance($t);` +
		`try{$a.SetAppAsDefaultAll('mpv');Write-Output 'ok'}catch{Write-Output ('falha: '+$_.Exception.Message)}`
	c := hiddenCmd("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	out, err := c.Output()
	if err != nil {
		logAppend("mpv: default via COM falhou: ", err.Error())
		return
	}
	logAppend("mpv: default via COM: ", strings.TrimSpace(string(out)))
}
