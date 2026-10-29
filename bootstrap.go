//go:build windows

package main

// bootstrap.go — resources the application embeds at compile time.
//
// The bundled MPV payload is NOT embedded: it is shipped by the Inno Setup
// installer as ordinary files under <install root>\mpv. Embedding ~127 MB in the
// executable would triple the download size of the application itself for no
// benefit, now that a real installer owns the file layout.
//
// Only the icon is embedded, because the tray needs it at runtime and reading
// it from disk would make a missing/relocated file break the tray icon.

import (
	"embed"
)

// iconAssets carries the multi-resolution application icon used by the tray.
// The same file is also installed next to the executable and referenced by the
// installer (SetupIconFile / UninstallDisplayIcon).
//
//go:embed kouki.ico
var iconAssets embed.FS

// appIconPNG is the 512 px master artwork, the source the build pipeline turns
// into kouki.ico. It is embedded as well so the window can paint the real mark
// in the header and set the same artwork as its title-bar icon. Both were
// missing: the header drew the letter "K" inside a coloured square, and walk
// registers its window class with a NULL hIcon, which leaves the title bar on
// the system default.
//
//go:embed assets/icon/icon-512.png
var appIconPNG embed.FS
