package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredMpvPipe(t *testing.T) {
	tests := []struct {
		name, config, want string
	}{
		{"global", "input-ipc-server=custom", "custom"},
		{"comment", "# input-ipc-server=ignored\ninput-ipc-server=real # note", "real"},
		{"different option", "input-ipc-server-extra=wrong", ""},
		{"profile only", "[movie]\ninput-ipc-server=wrong", ""},
		{"default section", "[movie]\ninput-ipc-server=wrong\n[default]\ninput-ipc-server=right", "right"},
		{"last global wins", "input-ipc-server=old\n[default]\ninput-ipc-server=new", "new"},
		{"empty disables", "input-ipc-server=old\ninput-ipc-server=", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configuredMpvPipe(tt.config); got != tt.want {
				t.Fatalf("configuredMpvPipe() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureMpvIpcConfigResetsProfile(t *testing.T) {
	t.Setenv("MPV_PIPE", "")
	root := t.TempDir()
	exe := filepath.Join(root, "mpv.exe")
	if err := os.WriteFile(exe, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	portable := filepath.Join(root, "portable_config")
	if err := os.Mkdir(portable, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(portable, "mpv.conf")
	original := "[movie]\r\nvolume=20\r\ninput-ipc-server=profile-only\r\n"
	if err := os.WriteFile(conf, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMpvIpcConfig(exe); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), original) {
		t.Fatal("existing user options were changed")
	}
	if got := configuredMpvPipe(string(first)); got != `\\.\pipe\mpv-pipe` {
		t.Fatalf("configured pipe = %q", got)
	}
	if err := EnsureMpvIpcConfig(exe); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("re-running IPC setup duplicated its config")
	}
}

func TestMpvPipeNameUsesSelectedConfig(t *testing.T) {
	t.Setenv("MPV_PIPE", "")
	root := t.TempDir()
	exe := filepath.Join(root, "mpv.exe")
	if err := os.WriteFile(exe, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	portable := filepath.Join(root, "portable_config")
	if err := os.Mkdir(portable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portable, "mpv.conf"), []byte("input-ipc-server=custom/subpipe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := selectedMpv()
	setSelectedMpv(exe)
	t.Cleanup(func() { setSelectedMpv(previous) })
	if got := mpvPipeName(); got != `\\.\pipe\custom\subpipe` {
		t.Fatalf("mpvPipeName() = %q", got)
	}
	if got := pipeLeaf(mpvPipeName()); got != `custom\subpipe` {
		t.Fatalf("pipeLeaf() = %q", got)
	}
}
