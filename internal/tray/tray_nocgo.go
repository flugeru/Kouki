//go:build linux && !cgo

package tray

import "sync"

// A headless fallback keeps the dashboard usable on Linux systems without
// AppIndicator libraries, and makes pure-Go cross compilation possible.
type MenuItem struct {
	ClickedCh chan struct{}
	mu        sync.Mutex
	checked   bool
	text      string
}

var quit = make(chan struct{})
var quitOnce sync.Once

func Available() bool { return false }

func Run(onReady func(), onExit func()) {
	if onReady != nil {
		onReady()
	}
	<-quit
	if onExit != nil {
		onExit()
	}
}
func Quit()             { quitOnce.Do(func() { close(quit) }) }
func SetIcon([]byte)    {}
func SetTooltip(string) {}
func AddMenuItem(title, tooltip string) *MenuItem {
	return &MenuItem{ClickedCh: make(chan struct{}), text: title}
}
func AddMenuItemCheckbox(title, tooltip string, checked bool) *MenuItem {
	return &MenuItem{ClickedCh: make(chan struct{}), text: title, checked: checked}
}
func AddSeparator()                       {}
func (m *MenuItem) SetTitle(value string) { m.mu.Lock(); m.text = value; m.mu.Unlock() }
func (m *MenuItem) SetTooltip(string)     {}
func (m *MenuItem) Enable()               {}
func (m *MenuItem) Disable()              {}
func (m *MenuItem) Check()                { m.mu.Lock(); m.checked = true; m.mu.Unlock() }
func (m *MenuItem) Uncheck()              { m.mu.Lock(); m.checked = false; m.mu.Unlock() }
func (m *MenuItem) Checked() bool         { m.mu.Lock(); defer m.mu.Unlock(); return m.checked }
func (m *MenuItem) AddSubMenuItem(title, tooltip string) *MenuItem {
	return AddMenuItem(title, tooltip)
}
func (m *MenuItem) AddSubMenuItemCheckbox(title, tooltip string, checked bool) *MenuItem {
	return AddMenuItemCheckbox(title, tooltip, checked)
}
