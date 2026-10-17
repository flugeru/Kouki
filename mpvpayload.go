package main

// mpvpayload.go — the MPV distribution that ships inside Kouki.exe.
//
// Startup behaviour requested for Kouki:
//
//  1. look for an MPV already installed on this machine (typically the build
//     people download from the project's GitHub releases);
//  2. if none is found, take the MPV bundled here and install it for the user,
//     already configured;
//  3. if one is found, only its mpv.conf needs fixing so the IPC channel opens.
//
// Because step 2 has to work from the running executable alone, the payload is
// embedded rather than left only in the Inno Setup package. That costs the
// executable ~127 MB; without it Kouki could not install anything by itself.

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mpvAssets is the bundled MPV tree: the binaries plus portable_config/, which
// carries the repaired mpv.conf and the user's presets, scripts and shaders.
//
//go:embed all:assets/mpv
var mpvAssets embed.FS

// BundledMpvInstallDir is where Kouki installs the MPV when it finds none.
//
// A real, conventional location rather than a folder inside Kouki's own
// install, so the player stays usable if Kouki is ever removed.
func BundledMpvInstallDir() string {
	return filepath.Join(localAppData(), "Programs", "mpv")
}

// mpvPayloadEntries lists the bundled files, sorted, with relative paths
// already slash-normalised for fs.FS.
func mpvPayloadEntries() ([]string, error) {
	var out []string
	err := fs.WalkDir(mpvAssets, "assets/mpv", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "assets/mpv/")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// InstallBundledMpv writes the embedded MPV into dir and guarantees its
// mpv.conf exposes the IPC channel.
//
// It is deliberately merge-style rather than destructive: files already present
// with the same size are skipped, so re-running after an update is cheap and
// never leaves a half-written player. A locked mpv.exe (the player running) is
// reported, not treated as fatal.
func InstallBundledMpv(dir string) (int, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = BundledMpvInstallDir()
	}
	logAppend("mpv: instalando payload em ", dir)

	entries, err := mpvPayloadEntries()
	if err != nil {
		return 0, fmt.Errorf("payload do MPV indisponivel: %w", err)
	}
	if len(entries) == 0 {
		return 0, fmt.Errorf("payload do MPV vazio")
	}

	var written, skipped int
	var locked []string

	for _, rel := range entries {
		data, err := fs.ReadFile(mpvAssets, "assets/mpv/"+rel)
		if err != nil {
			return written, fmt.Errorf("leitura de %s: %w", rel, err)
		}

		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return written, err
		}

		// Skip identical files so an upgrade only rewrites what changed.
		if info, err := os.Stat(target); err == nil && info.Size() == int64(len(data)) {
			skipped++
			continue
		}

		if err := os.WriteFile(target, data, 0o600); err != nil {
			// A running mpv.exe cannot be overwritten; note it and carry on so the
			// rest of the tree still lands.
			if strings.HasSuffix(strings.ToLower(rel), ".exe") {
				locked = append(locked, filepath.Base(rel))
				continue
			}
			return written, fmt.Errorf("escrita de %s: %w", rel, err)
		}
		written++
	}

	if len(locked) > 0 {
		logAppend("mpv: em uso: ", strings.Join(locked, ", "))
		return written, fmt.Errorf("%s esta em uso; feche o player e tente de novo", strings.Join(locked, ", "))
	}

	logAppend("mpv: payload ok — ", fmt.Sprint(written), " escritos, ", fmt.Sprint(skipped), " inalterados")
	return written, nil
}

// InstallAndConfigureMpv installs the bundled MPV into dir (when nothing is
// installed yet) and makes sure the resulting config opens the IPC pipe.
func InstallAndConfigureMpv(dir string) (exe string, installed bool, err error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = BundledMpvInstallDir()
	}

	exe = filepath.Join(dir, "mpv.exe")
	if !fileExists(exe) {
		if _, err := InstallBundledMpv(dir); err != nil {
			return "", false, err
		}
		installed = true
	}

	if !fileExists(exe) {
		return "", false, fmt.Errorf("mpv.exe nao apareceu em %s", dir)
	}
	logAppend("mpv: exe em ", exe)

	// The bundled mpv.conf already carries the IPC line, but an mpv that has been
	// edited may have lost it; ensure it either way.
	if err := EnsureMpvIpcConfig(exe); err != nil {
		return exe, installed, err
	}

	// Kouki just laid the player down: make Windows treat it as an installed
	// program rather than a loose executable. Only on a real install, so an mpv
	// that was already there is not re-registered on every startup.
	if installed {
		registerMpvForWindows(exe)
	}
	return exe, installed, nil
}
