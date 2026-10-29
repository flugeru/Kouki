//go:build windows

package main

import (
	"testing"

	"github.com/lxn/walk"
)

func TestShiftCachedLayoutKeepsHitTestingAligned(t *testing.T) {
	u := &uiCanvas{
		items: []uiItem{
			{kind: itButton, enabled: true, rect: walk.Rectangle{X: 10, Y: 100, Width: 80, Height: 24}},
			{kind: itButton, enabled: true, rect: walk.Rectangle{X: 10, Y: 150, Width: 80, Height: 24}},
		},
		cards:          []uiCard{{rect: walk.Rectangle{X: 0, Y: 90, Width: 100, Height: 100}}},
		headerRect:     walk.Rectangle{X: 10, Y: 10, Width: 80, Height: 40},
		headerDividerY: 55,
	}
	if got := u.hitAt(20, 105); got != 0 {
		t.Fatalf("initial hit = %d, want 0", got)
	}
	u.shiftCachedLayout(-50)
	if got := u.hitAt(20, 105); got != 1 {
		t.Fatalf("hit after scroll = %d, want 1", got)
	}
	if u.cards[0].rect.Y != 40 || u.headerRect.Y != -40 || u.headerDividerY != 5 {
		t.Fatal("cached card/header geometry did not follow scrolled pixels")
	}
}
