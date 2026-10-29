//go:build windows

package main

// mpvdetect.go — locating an mpv installation.
//
// Kouki ships its own mpv (installed by the Inno Setup package into
// <install root>\mpv), but many people already have mpv somewhere else and would
// rather use that copy: it may be a newer build, or carry their own config.
//
// This file implements the search. It is read-only — it never downloads or
// copies anything — and reports candidates so the GUI can present the choice.

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// mpvCandidate is one mpv installation that was found.
type mpvCandidate struct {
	Path    string // full path to mpv.exe
	Version string // first line of `mpv --version`, when it could be read
	Config  string // directory holding mpv.conf, when one was found
}

// mpvStandardDirs returns the folders mpv is conventionally installed into on
// Windows, most likely first.
func mpvStandardDirs() []string {
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		dirs = append(dirs, p)
	}

	// Portable / manual installs people tend to keep.
	add(os.Getenv("MPV_PATH"))
	add(filepath.Join(os.Getenv("USERPROFILE"), "Documents", "mpv"))
	add(filepath.Join(os.Getenv("USERPROFILE"), "Downloads", "mpv"))

	// Per-user and machine-wide program locations.
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		add(filepath.Join(local, "Programs", "mpv"))
		add(filepath.Join(local, "Microsoft", "WindowsApps", "mpv"))
		add(filepath.Join(local, "scoop", "apps", "mpv", "current"))
	}
	if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
		add(filepath.Join(programFiles, "mpv"))
		add(filepath.Join(programFiles, "mpv.net"))
	}
	if programFilesX86 := os.Getenv("ProgramFiles(x86)"); programFilesX86 != "" {
		add(filepath.Join(programFilesX86, "mpv"))
	}
	if choco := os.Getenv("ChocolateyInstall"); choco != "" {
		add(filepath.Join(choco, "bin"))
		add(filepath.Join(choco, "lib", "mpv", "tools"))
	}

	// The directory Kouki itself was installed into.
	add(bundledMpvDir(InstallRoot()))

	return dirs
}

// mpvOnPath returns every mpv.exe reachable through PATH, which is how a
// scoop/choco/WindowsApps install is normally invoked.
func mpvOnPath() []string {
	var found []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		dir = strings.Trim(strings.TrimSpace(dir), `"`)
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "mpv.exe")
		if fileExists(candidate) {
			found = append(found, candidate)
		}
	}
	return found
}

// mpvConfigNear returns the mpv.conf that belongs to an mpv.exe, if any.
//
// mpv loads portable_config/ next to the binary, then %APPDATA%\mpv. Reporting
// which one applies is what tells the user why their settings did or did not
// take effect.
func mpvConfigNear(exe string) string {
	path := mpvConfigPathFor(exe)
	if fileExists(path) {
		return path
	}
	return ""
}

// mpvConfigPathFor returns the mpv.conf that applies to an mpv.exe, or "" if
// there is none yet.
//
// portable_config/mpv.conf next to the binary wins because mpv loads it
// automatically and it travels with the installation.
func mpvConfigPathFor(exe string) string {
	dir := filepath.Dir(exe)
	portableDir := filepath.Join(dir, "portable_config")
	// The presence of portable_config disables the roaming config even when
	// mpv.conf has not been created inside it yet.
	if st, err := os.Stat(portableDir); err == nil && st.IsDir() {
		return filepath.Join(portableDir, "mpv.conf")
	}
	if home := strings.TrimSpace(os.Getenv("MPV_HOME")); home != "" {
		return filepath.Join(home, "mpv.conf")
	}
	// The roaming config overrides a file beside mpv.exe. Prefer to update it
	// when both exist, so the setting Kouki reads is the setting mpv uses.
	if roaming := os.Getenv("APPDATA"); roaming != "" {
		user := filepath.Join(roaming, "mpv", "mpv.conf")
		if fileExists(user) {
			return user
		}
		beside := filepath.Join(dir, "mpv.conf")
		if fileExists(beside) {
			return beside
		}
		return user
	}
	beside := filepath.Join(dir, "mpv.conf")
	if fileExists(beside) {
		return beside
	}
	return filepath.Join(portableDir, "mpv.conf")
}

// ipcPipeLine is the single line appended to an existing user config: the one
// option Kouki depends on, without which mpv never opens the pipe. It is
// deliberately a lone line rather than a block, so nothing the user wrote is
// displaced or reformatted. It follows MPV_PIPE when that is set, so the line
// written here and the pipe the poll loop dials cannot disagree.
func ipcPipeLine(pipe string) string { return "input-ipc-server=" + pipe }

// configHasIPC reports whether a config already enables the IPC server. A line
// that is commented out does not count: matching the bare text anywhere in the
// file made `#input-ipc-server=...` look configured, and Kouki then never fixed
// a player that was in fact deaf.
func configuredMpvPipe(conf string) string {
	pipe := ""
	inProfile := false
	for _, line := range strings.Split(conf, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProfile = !strings.EqualFold(strings.TrimSpace(line[1:len(line)-1]), "default")
			continue
		}
		if inProfile {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "input-ipc-server" {
			// mpv uses the last normal option, including an empty value.
			pipe = strings.TrimSpace(value)
		}
	}
	return pipe
}

func configHasIPC(conf string) bool { return configuredMpvPipe(conf) != "" }

func configEndsInProfile(conf string) bool {
	inProfile := false
	for _, line := range strings.Split(conf, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProfile = !strings.EqualFold(strings.TrimSpace(line[1:len(line)-1]), "default")
		}
	}
	return inProfile
}

// EnsureMpvIpcConfig guarantees that an mpv installation exposes the IPC pipe
// Kouki reads.
//
// It never rewrites existing settings: if a config is already present it is only
// appended to, and only when the option is genuinely absent. When there is no
// config at all, a minimal portable_config/mpv.conf is created next to the
// binary, which is where mpv looks first.
func EnsureMpvIpcConfig(exe string) error {
	exe = strings.TrimSpace(exe)
	if exe == "" || !fileExists(exe) {
		return errors.New("mpv.exe nao encontrado")
	}

	logAppend("mpv: verificando IPC em ", exe)
	path := mpvConfigPathFor(exe)
	desired := `\\.\pipe\mpv-pipe`
	if env := strings.TrimSpace(os.Getenv("MPV_PIPE")); env != "" {
		desired = normalizeMpvPipe(env)
	}
	if !fileExists(path) {
		logAppend("mpv: sem config; criando ", path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}

		body := "# mpv.conf criado pelo Kouki para expor o canal de IPC.\r\n" +
			"# Preserva as opcoes do usuario; apenas habilita a leitura de estado.\r\n" +
			ipcPipeLine(desired) + "\r\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
		logAppend("mpv: config criado em ", path)
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	logAppend("mpv: config em ", path)
	configured := configuredMpvPipe(string(data))
	if configured != "" {
		if os.Getenv("MPV_PIPE") == "" || strings.EqualFold(normalizeMpvPipe(configured), desired) {
			return nil // honour an existing custom pipe
		}
	}

	// Append only the pipe line, on its own line, at the very end: nothing the
	// user wrote is removed, reordered or reformatted. A trailing newline is
	// guaranteed first so the line never merges with the user's last one.
	body := string(data)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\r\n"
	}
	if configEndsInProfile(body) {
		body += "[default]\r\n"
	}
	body += ipcPipeLine(desired) + "\r\n"

	return os.WriteFile(path, []byte(body), 0o600)
}

// FindMpvCandidates searches the standard locations plus PATH and returns every
// distinct installation, most likely first.
//
// It never touches the network and never writes anything, so it is safe to call
// on demand from the GUI.
func FindMpvCandidates() []mpvCandidate {
	seen := make(map[string]bool)
	var out []mpvCandidate

	add := func(exe string) {
		if exe == "" || !fileExists(exe) {
			return
		}
		key := strings.ToLower(exe)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, mpvCandidate{
			Path:    exe,
			Version: mpvVersionLine(exe),
			Config:  mpvConfigNear(exe),
		})
	}

	// Kouki's own copy comes first, in this order:
	//   1. the one the installer put next to the app
	//   2. the one Kouki installed by itself under %LOCALAPPDATA%\Programs\mpv
	// Either way it is the copy whose bundled config carries the user's presets,
	// so it wins over any stray mpv lying around in Downloads.
	add(FindBundledMpv())
	add(filepath.Join(BundledMpvInstallDir(), "mpv.exe"))

	for _, dir := range mpvStandardDirs() {
		add(filepath.Join(dir, "mpv.exe"))
	}
	for _, exe := range mpvOnPath() {
		add(exe)
	}

	return out
}

// DefaultMpvPath picks the mpv Kouki should use when the user has not chosen
// one explicitly: Kouki's own copy if present, otherwise the first candidate.
func DefaultMpvPath() string {
	if p := FindBundledMpv(); p != "" {
		return p
	}
	if p := filepath.Join(BundledMpvInstallDir(), "mpv.exe"); fileExists(p) {
		return p
	}
	if c := FindMpvCandidates(); len(c) > 0 {
		return c[0].Path
	}
	return ""
}

// ---------------------------------------------------------------------------
// user override
// ---------------------------------------------------------------------------

// selectedMpvPath is the mpv the user picked in the GUI, if any. It wins over
// every other source, so a power user can point Kouki at their own build.
var selectedMpvPath = func() string {
	return strings.TrimSpace(os.Getenv("KOUKI_MPV"))
}()
var selectedMpvMu sync.RWMutex

func selectedMpv() string {
	selectedMpvMu.RLock()
	defer selectedMpvMu.RUnlock()
	return selectedMpvPath
}

// resolveMpvPath is the single place that decides which mpv.exe Kouki talks to.
// Everything else (launching the player, the About dialog, the GUI status)
// goes through it, so the choice can never disagree between features.
func resolveMpvPath() string {
	selected := selectedMpv()
	if selected != "" && fileExists(selected) {
		return selected
	}
	return DefaultMpvPath()
}

// setSelectedMpv records the user's choice for this session.
func setSelectedMpv(path string) {
	selectedMpvMu.Lock()
	selectedMpvPath = strings.TrimSpace(path)
	selectedMpvMu.Unlock()
}

// describeMpvPath is a one-line summary for the GUI status area.
func describeMpvPath(path string) string {
	if path == "" {
		return "nenhum MPV encontrado"
	}
	label := path
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			label = "~" + string(filepath.Separator) + rel
		}
	}
	if samePath(path, FindBundledMpv()) || samePath(path, filepath.Join(BundledMpvInstallDir(), "mpv.exe")) {
		return label + " (do Kouki)"
	}
	return label
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, err1 := filepath.Abs(a)
	rb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return strings.EqualFold(a, b)
	}
	return strings.EqualFold(ra, rb)
}

// mpvPipeName derives the IPC pipe Kouki expects. It matches the default in the
// bundled mpv.conf so an external player with the stock config still works.
func mpvPipeName() string {
	if v := strings.TrimSpace(os.Getenv("MPV_PIPE")); v != "" {
		return normalizeMpvPipe(v)
	}
	if exe := resolveMpvPath(); exe != "" {
		if data, err := os.ReadFile(mpvConfigPathFor(exe)); err == nil {
			if pipe := configuredMpvPipe(string(data)); pipe != "" {
				return normalizeMpvPipe(pipe)
			}
		}
	}
	return `\\.\pipe\mpv-pipe`
}

// normalizeMpvPipe mirrors mpv's Windows rule: paths without the named-pipe
// prefix are placed under \\.\pipe\, retaining any nested components.
func normalizeMpvPipe(pipe string) string {
	pipe = strings.TrimSpace(strings.Trim(pipe, `"`))
	if strings.HasPrefix(strings.ToLower(pipe), `\\.\pipe\`) {
		return `\\.\pipe\` + pipe[len(`\\.\pipe\`):]
	}
	pipe = strings.TrimLeft(strings.ReplaceAll(pipe, "/", `\`), `\`)
	return `\\.\pipe\` + pipe
}

// pipeLeaf removes the named-pipe namespace while retaining nested components
// for --input-ipc-server (which Launch prefixes itself).
func pipeLeaf(pipe string) string {
	pipe = normalizeMpvPipe(pipe)
	if strings.HasPrefix(strings.ToLower(pipe), `\\.\pipe\`) {
		return pipe[len(`\\.\pipe\`):]
	}
	return pipe
}

// sortCandidates keeps the bundled copy first, then alphabetical, so the GUI
// list order is stable between runs.
func sortCandidates(list []mpvCandidate) {
	// Resolved once: the comparator runs O(n log n) times and each lookup stats
	// the disk.
	owned := []string{FindBundledMpv(), filepath.Join(BundledMpvInstallDir(), "mpv.exe")}
	selected := selectedMpv()
	rank := func(p string) int {
		if selected != "" && samePath(p, selected) {
			return 0 // the user's explicit choice always wins
		}
		for _, o := range owned {
			if samePath(p, o) {
				return 1 // Kouki's own copies, which carry the bundled presets
			}
		}
		return 2
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i].Path), rank(list[j].Path)
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(list[i].Path) < strings.ToLower(list[j].Path)
	})
}
