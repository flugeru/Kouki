package main

// mpvpayload.go — optional MPV payload support.
//
// Startup behaviour requested for Kouki:
//
//  1. look for an MPV already installed on this machine (typically the build
//     people download from the project's GitHub releases);
//  2. if none is found, take the MPV bundled here and install it for the user,
//     already configured;
//  3. if one is found, only its mpv.conf needs fixing so the IPC channel opens.
//
// Source checkouts contain the configuration tree but may omit mpv.exe. Release
// builds can embed the complete player after tools/prepare-mpv.ps1 is run.

import (
	"bytes"
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
// It is merge-style: byte-identical files are skipped, and changed files are
// replaced through a temporary file so an interrupted write does not truncate
// the installed player. A locked mpv.exe is reported as an error.
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

		// Size alone cannot prove equality: a damaged or older file can have
		// exactly the same length.
		if info, err := os.Stat(target); err == nil && info.Size() == int64(len(data)) {
			if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
				skipped++
				continue
			}
		}

		if err := replaceMpvFile(target, data); err != nil {
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

func replaceMpvFile(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".kouki-mpv-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
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
		// A build made without a prepared payload carries the configuration but
		// not the player. Installing it anyway left a half-populated folder behind
		// and then failed with "mpv.exe não apareceu"; say what is actually wrong.
		if _, err := fs.Stat(mpvAssets, "assets/mpv/mpv.exe"); err != nil {
			return "", false, fmt.Errorf("este build do %s não traz o MPV embutido; instale o MPV pelo instalador ou aponte para um mpv.exe existente", appName)
		}
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
