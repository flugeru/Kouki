//go:build linux && cgo

package tray

import "github.com/getlantern/systray"

type MenuItem = systray.MenuItem

func Available() bool { return true }

func Run(onReady func(), onExit func())           { systray.Run(onReady, onExit) }
func Quit()                                       { systray.Quit() }
func SetIcon(icon []byte)                         { systray.SetIcon(icon) }
func SetTooltip(value string)                     { systray.SetTooltip(value) }
func AddMenuItem(title, tooltip string) *MenuItem { return systray.AddMenuItem(title, tooltip) }
func AddMenuItemCheckbox(title, tooltip string, checked bool) *MenuItem {
	return systray.AddMenuItemCheckbox(title, tooltip, checked)
}
func AddSeparator() { systray.AddSeparator() }
