package main

import (
	"context"
	"testing"
)

func TestOAuthLoginSlotsShareWindowAndTrayState(t *testing.T) {
	t.Cleanup(func() {
		loginCancelMu.Lock()
		if loginCancelFunc != nil {
			loginCancelFunc()
			loginCancelFunc = nil
		}
		loginCancelMu.Unlock()
		malLoginCancelMu.Lock()
		if malLoginCancelFunc != nil {
			malLoginCancelFunc()
			malLoginCancelFunc = nil
		}
		malLoginCancelMu.Unlock()
	})

	aniCtx, aniGen, canceled := startOrCancelAniLogin(context.Background())
	if canceled || aniCtx == nil {
		t.Fatal("first AniList login was not started")
	}
	malCtx, malGen, canceled := startOrCancelMALLogin(context.Background())
	if canceled || malCtx == nil {
		t.Fatal("first MAL login was not started")
	}

	if ctx, _, canceled := startOrCancelAniLogin(context.Background()); !canceled || ctx != nil {
		t.Fatal("second AniList click started a rival callback server")
	}
	if aniCtx.Err() != context.Canceled {
		t.Fatal("second AniList click did not cancel the first attempt")
	}
	if malCtx.Err() != nil {
		t.Fatal("canceling AniList affected MAL")
	}
	if ctx, _, canceled := startOrCancelAniLogin(context.Background()); !canceled || ctx != nil {
		t.Fatal("AniList restarted before the canceled callback server closed")
	}
	if !finishAniLogin(aniGen) {
		t.Fatal("completed AniList attempt did not release its slot")
	}
	aniNext, aniNextGen, canceled := startOrCancelAniLogin(context.Background())
	if canceled || aniNext == nil || aniNextGen == aniGen {
		t.Fatal("AniList could not restart after callback shutdown")
	}
	if finishAniLogin(aniGen) || aniNext.Err() != nil {
		t.Fatal("old AniList completion cleared the newer attempt")
	}
	finishAniLogin(aniNextGen)

	if ctx, _, canceled := startOrCancelMALLogin(context.Background()); !canceled || ctx != nil {
		t.Fatal("second MAL click started a rival callback server")
	}
	if malCtx.Err() != context.Canceled {
		t.Fatal("second MAL click did not cancel the first attempt")
	}
	if ctx, _, canceled := startOrCancelMALLogin(context.Background()); !canceled || ctx != nil {
		t.Fatal("MAL restarted before the canceled callback server closed")
	}
	if !finishMALLogin(malGen) {
		t.Fatal("completed MAL attempt did not release its slot")
	}
	malNext, malNextGen, canceled := startOrCancelMALLogin(context.Background())
	if canceled || malNext == nil || malNextGen == malGen {
		t.Fatal("MAL could not restart after callback shutdown")
	}
	if finishMALLogin(malGen) || malNext.Err() != nil {
		t.Fatal("old MAL completion cleared the newer attempt")
	}
	finishMALLogin(malNextGen)
}
