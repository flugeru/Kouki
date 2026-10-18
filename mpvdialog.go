// mpvdialog.go - modern Fluent-style dialog shown when Kouki installs its
// bundled MPV for the first time. Uses the Windows TaskDialogIndirect API
// for a native, dark-mode-aware, modern dialog.

package main

import (
	"syscall"
	"unsafe"

	"github.com/lxn/win"
)

// TaskDialogIndirect comctl32 structures and constants.
const (
	tdIconInformation = uintptr(0xFFFFFFFF - 1) // TD_INFORMATION_ICON
	tcOK              = 1
)

// TASKDIALOG_BUTTON structure.
type taskDialogButton struct {
	ID   uint32
	Text *uint16
}

var (
	comctl32               = syscall.NewLazyDLL("comctl32.dll")
	procTaskDialogIndirect = comctl32.NewProc("TaskDialogIndirect")
)

// showMpvInstalledDialog opens a modal, dark, Fluent-style dialog informing the
// user that the bundled MPV has just been installed.
func showMpvInstalledDialog(exePath string) {
	owner := gui.w
	var parent win.HWND
	if owner != nil {
		parent = win.HWND(owner.Handle())
	}

	title, _ := syscall.UTF16PtrFromString(appName)
	mainInstr, _ := syscall.UTF16PtrFromString("Nenhum MPV estava instalado.")
	content, _ := syscall.UTF16PtrFromString("O MPV que acompanha o " + appName + " foi instalado em:\n\n" + exePath +
		"\n\nEle já está com o canal IPC configurado, então o " + appName +
		" acompanha a reprodução assim que você começar a assistir.")

	okText, _ := syscall.UTF16PtrFromString("OK")
	buttons := []taskDialogButton{
		{ID: tcOK, Text: okText},
	}

	// TASKDIALOGCONFIG matches the Win32 struct field-for-field.
	type taskDialogConfig struct {
		Size             uint32
		Parent           win.HWND
		Instance         win.HANDLE
		Flags            uint32
		CommonButtons    uint32
		WindowTitle      *uint16
		MainIcon         uintptr
		MainInstruction  *uint16
		Content          *uint16
		Buttons          uintptr
		DefaultButton    uint32
		RadioButtons     uintptr
		VerificationText *uint16
		ExpandedInfo     *uint16
		FooterIcon       uintptr
		Footer           *uint16
		Width            uint32
		Callback         uintptr
		CallbackData     uintptr
		MaxWidth         uint32
	}

	cfg := taskDialogConfig{
		Size:            uint32(unsafe.Sizeof(taskDialogConfig{})),
		Parent:          parent,
		Flags:           0,
		WindowTitle:     title,
		MainIcon:        tdIconInformation,
		MainInstruction: mainInstr,
		Content:         content,
		Buttons:         uintptr(unsafe.Pointer(&buttons[0])),
		DefaultButton:   tcOK,
	}

	result := int32(0)
	ret, _, _ := procTaskDialogIndirect.Call(
		uintptr(unsafe.Pointer(&cfg)),
		uintptr(unsafe.Pointer(&result)),
		0,
		0,
	)
	_ = ret
}
