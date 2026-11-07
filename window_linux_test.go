//go:build linux

package main

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutostartUsesOuterAppImage(t *testing.T) {
	config := t.TempDir()
	image := filepath.Join(config, "Meu Kouki.AppImage")
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("APPIMAGE", image)
	if err := setLinuxAutostart(true); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(config, "autostart", "kouki.desktop")
	content, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `Exec="`+image+`" --tray`) {
		t.Fatalf("autostart deve usar o AppImage externo, não o executável temporário: %s", content)
	}
	if err := setLinuxAutostart(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("autostart não foi removido: %v", err)
	}
}

func TestDashboardRejectsCrossOriginActions(t *testing.T) {
	linuxWindow.mu.Lock()
	oldURL, oldToken := linuxWindow.url, linuxWindow.token
	linuxWindow.url, linuxWindow.token = "http://127.0.0.1:45678/", "test-token"
	linuxWindow.mu.Unlock()
	t.Cleanup(func() {
		linuxWindow.mu.Lock()
		linuxWindow.url, linuxWindow.token = oldURL, oldToken
		linuxWindow.mu.Unlock()
	})

	check := func(host, origin, token string, want int) {
		t.Helper()
		req := httptest.NewRequest("POST", "http://127.0.0.1:45678/api/action", strings.NewReader(`{"action":"unknown"}`))
		req.Host = host
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Kouki-Token", token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		linuxWindowHandler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("host=%q origin=%q token=%q: status %d, quer %d", host, origin, token, rec.Code, want)
		}
	}
	check("evil.example", "http://127.0.0.1:45678", "test-token", 403)
	check("127.0.0.1:45678", "http://evil.example", "test-token", 403)
	check("127.0.0.1:45678", "http://127.0.0.1:45678", "wrong", 403)
	// A valid same-origin request reaches the action router and receives its
	// expected unknown-action response rather than an authorization error.
	check("127.0.0.1:45678", "http://127.0.0.1:45678", "test-token", 400)
}
