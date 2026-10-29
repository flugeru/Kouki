package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOverrideStoreConcurrentSetPersistsEveryKey(t *testing.T) {
	const count = 32
	store := &overrideStore{Path: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.Set(fmt.Sprintf("show-%d", i), i+1)
		}(i)
	}
	wg.Wait()

	loaded := &overrideStore{Path: store.Path}
	loaded.Load()
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("show-%d", i)
		if got := loaded.Get(key); got != i+1 {
			t.Fatalf("%s: ID salvo = %d, esperado %d", key, got, i+1)
		}
	}
}

func TestAuthStoreKeepsDefaultsAndTrackingPreference(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "auth.json"), []byte(`{"access_token":"ani","mal_access_token":"mal","mal_tracking":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := &authStore{Path: path}
	store.Load()
	if !store.WarnFiller {
		t.Fatal("warn_filler ausente deveria manter o padrão ativo")
	}
	if store.MALTracking {
		t.Fatal("preferência de tracking do MAL deveria ser preservada")
	}

	store.ClearMAL()
	loaded := &authStore{Path: path}
	loaded.Load()
	if loaded.MALTracking {
		t.Fatal("sair do MAL mudou a preferência de tracking")
	}
	if loaded.AccessToken != "ani" || loaded.MALAccessToken != "" {
		t.Fatalf("tokens inesperados após sair do MAL: AniList=%q MAL=%q", loaded.AccessToken, loaded.MALAccessToken)
	}
}
