package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type mpvCandidate struct {
	Path    string
	Version string
	Config  string
}

func mpvUserConfigPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "mpv", "mpv.conf")
}

func mpvConfigNear(exe string) string {
	path := mpvUserConfigPath()
	if fileExists(path) {
		return path
	}
	return ""
}

func mpvConfigPathFor(exe string) string { return mpvUserConfigPath() }

func ipcPipeLine() string { return "input-ipc-server=" + mpvPipeName() }

func configuredMpvPipe(conf string) string {
	value := ""
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			break
		}
		key, path, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "input-ipc-server" {
			path = strings.SplitN(path, " #", 2)[0]
			value = strings.Trim(strings.TrimSpace(path), `"'`)
		}
	}
	return value
}

func configHasIPC(conf string) bool {
	return configuredMpvPipe(conf) != ""
}

// EnsureMpvIpcConfig makes an already installed mpv visible to Kouki even when
// the user starts it outside Kouki. It only changes the user's mpv.conf.
func EnsureMpvIpcConfig(exe string) error {
	if _, err := resolveExecutable(exe); err != nil {
		return errors.New("mpv não encontrado")
	}
	path := mpvUserConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if existing := configuredMpvPipe(string(data)); existing != "" {
		if !filepath.IsAbs(existing) {
			return fmt.Errorf("input-ipc-server em %s usa caminho relativo; defina um socket absoluto para o Kouki", path)
		}
		if override := strings.TrimSpace(os.Getenv("MPV_PIPE")); override != "" && mpvPipeName() != existing {
			return fmt.Errorf("MPV_PIPE (%s) difere do socket configurado em %s (%s)", override, path, existing)
		}
		return nil
	}
	line := []byte("# Kouki: socket IPC para acompanhar o MPV\n" + ipcPipeLine() + "\n")
	data = append(line, data...)
	return writeFileAtomic(path, data, 0o600)
}

func resolveExecutable(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("caminho vazio")
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func FindMpvCandidates() []mpvCandidate {
	seen := map[string]bool{}
	var out []mpvCandidate
	add := func(name string) {
		path, err := resolveExecutable(name)
		if err != nil || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, mpvCandidate{Path: path, Version: mpvVersionLine(path), Config: mpvConfigNear(path)})
	}
	add(os.Getenv("KOUKI_MPV"))
	add(os.Getenv("MPV_PATH"))
	add("mpv")
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		add(filepath.Join(dir, "mpv"))
	}
	return out
}

func DefaultMpvPath() string {
	if list := FindMpvCandidates(); len(list) > 0 {
		return list[0].Path
	}
	return ""
}

var selectedMpv = struct {
	mu   sync.RWMutex
	path string
}{path: strings.TrimSpace(os.Getenv("KOUKI_MPV"))}

func resolveMpvPath() string {
	selectedMpv.mu.RLock()
	chosen := selectedMpv.path
	selectedMpv.mu.RUnlock()
	if chosen != "" {
		if p, err := resolveExecutable(chosen); err == nil {
			return p
		}
	}
	return DefaultMpvPath()
}

func setSelectedMpv(path string) {
	selectedMpv.mu.Lock()
	selectedMpv.path = strings.TrimSpace(path)
	selectedMpv.mu.Unlock()
}

func describeMpvPath(path string) string {
	if path == "" {
		return "nenhum MPV encontrado"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "~" + string(filepath.Separator) + rel
		}
	}
	return path
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	return aErr == nil && bErr == nil && aAbs == bAbs
}

func mpvPipeName() string {
	if path := strings.TrimSpace(os.Getenv("MPV_PIPE")); path != "" {
		if !filepath.IsAbs(path) {
			if absolute, err := filepath.Abs(path); err == nil {
				return absolute
			}
		}
		return path
	}
	if data, err := os.ReadFile(mpvUserConfigPath()); err == nil {
		if path := configuredMpvPipe(string(data)); filepath.IsAbs(path) {
			return path
		}
	}
	dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if dir == "" {
		dir = ConfigDir()
	}
	return filepath.Join(dir, fmt.Sprintf("kouki-mpv-%d.sock", os.Getuid()))
}

// On Linux the full Unix socket path must be passed to mpv.
func pipeLeaf(pipe string) string { return pipe }

func sortCandidates(list []mpvCandidate) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Path < list[j].Path })
}
