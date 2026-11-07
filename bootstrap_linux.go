//go:build linux

package main

import "embed"

// The PNG is embedded for the system tray and local dashboard. The AppImage
// also installs it as the desktop icon.
//
//go:embed assets/icon/icon-512.png
var appIconPNG embed.FS
