package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	appName      = "Kouki"
	appVersion   = "1.1.0"
	appTagline   = "Contas, reprodução e progresso"
	iconFileName = "kouki.png"
	legacyName   = "Koushin"
)

func InstallRoot() string {
	if p := strings.TrimSpace(os.Getenv("APPDIR")); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func ConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, appName)
}

func MigrateLegacyConfig() error {
	old := filepath.Join(filepath.Dir(ConfigDir()), legacyName)
	entries, err := os.ReadDir(old)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		dst := filepath.Join(ConfigDir(), entry.Name())
		if fileExists(dst) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(old, entry.Name()))
		if err == nil {
			_ = os.WriteFile(dst, data, 0o600)
		}
	}
	return nil
}

func removeLegacyRunKey() error { return nil }

func singleInstancePath() string {
	return filepath.Join(ConfigDir(), "kouki.sock")
}

func acquireSingleInstanceGuard() (func(), error) {
	path := singleInstancePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(filepath.Dir(path)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("diretório de configurações inseguro")
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	listen := func() (net.Listener, error) { return net.Listen("unix", path) }
	ln, err := listen()
	if err != nil {
		if conn, dialErr := net.DialTimeout("unix", path, 300*time.Millisecond); dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("outra instância do Kouki já está em execução")
		}
		// A previous process may have crashed and left the socket pathname.
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, err
		}
		ln, err = listen()
		if err != nil {
			return nil, err
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
			showMainWindow()
		}
	}()
	return func() { _ = ln.Close(); _ = os.Remove(path) }, nil
}

func openInstallFolder() { openFolder(InstallRoot()) }

func installDirOrRunning() string { return InstallRoot() }

func mpvVersionLine(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "--version").Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(line) > 90 {
		line = line[:90] + "…"
	}
	return line
}
