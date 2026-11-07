package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMpvUnixIPC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		for {
			var req struct {
				Command   []any `json:"command"`
				RequestID int64 `json:"request_id"`
			}
			if err := dec.Decode(&req); err != nil {
				serverDone <- nil
				return
			}
			if err := enc.Encode(map[string]any{"event": "property-change", "name": "pause", "data": false}); err != nil {
				serverDone <- err
				return
			}
			if err := enc.Encode(map[string]any{"request_id": req.RequestID, "error": "success", "data": "0.40"}); err != nil {
				serverDone <- err
				return
			}
		}
	}()
	conn, err := dialMpv(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, err := conn.request("get_property", "mpv-version")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"0.40"` {
		t.Fatalf("resposta inesperada: %s", data)
	}
	if len(conn.drainEvents()) != 2 {
		t.Fatal("eventos assíncronos não foram enfileirados")
	}
	_ = conn.Close()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("servidor IPC não terminou")
	}
}

func TestMpvConfigPreservesExistingSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MPV_PIPE", "")
	conf := filepath.Join(dir, "mpv", "mpv.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "custom.sock")
	if err := os.WriteFile(conf, []byte("input-ipc-server="+existing+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mpvPipeName(); got != existing {
		t.Fatalf("socket escolhido = %q, quer %q", got, existing)
	}
	before, _ := os.ReadFile(conf)
	if err := EnsureMpvIpcConfig("/bin/true"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(conf)
	if string(before) != string(after) {
		t.Fatal("configuração existente do MPV foi alterada")
	}
}

func TestMpvConfigCreatesOneSocketLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MPV_PIPE", filepath.Join(dir, "mpv.sock"))
	if err := EnsureMpvIpcConfig("/bin/true"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMpvIpcConfig("/bin/true"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "mpv", "mpv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "input-ipc-server=") != 1 {
		t.Fatalf("configuração duplicada: %s", data)
	}
}

func TestMpvConfigIgnoresProfileSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MPV_PIPE", filepath.Join(dir, "kouki.sock"))
	conf := filepath.Join(dir, "mpv", "mpv.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := "[other]\ninput-ipc-server=/tmp/other.sock\n"
	if err := os.WriteFile(conf, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMpvIpcConfig("/bin/true"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# Kouki:") || !strings.HasSuffix(string(data), profile) {
		t.Fatalf("configuração do perfil foi alterada: %s", data)
	}
}
