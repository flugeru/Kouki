package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceMpvFileReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mpv.conf")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := replaceMpvFile(target, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("updated file = %q, want new", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
