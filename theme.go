package main

// theme.go — dark appearance for the two surfaces Kouki owns: the local web
// pages it opens in the browser (OAuth callbacks, anime selectors,
// Simulwatching) and the native main window.
//
// Browser pages
// -------------
// Those pages are hand-written HTML scattered across main.go. Rather than
// pasting a stylesheet into each one, withDarkPage wraps the HTTP handler and
// injects localPageCSS immediately before </head> — the last thing the parser
// sees, so it wins over whatever inline rules the handler wrote.
//
// Native window
// -------------
// walk has no dark mode of its own, so the main window opts into Windows'
// immersive dark mode (Win10 1809+) and paints its own dark surface. Every
// entry point is best-effort: on an older system the window simply keeps the
// light theme instead of failing to open.

import (
	"bytes"
	"net/http"
	"strings"
	"unsafe"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
)

// ptr32 is a small helper for the DwmSetWindowAttribute call, whose signature
// takes a raw pointer plus a byte size.
func ptr32(v *int32) unsafe.Pointer { return unsafe.Pointer(v) }

// ---------------------------------------------------------------------------
// browser pages
// ---------------------------------------------------------------------------

// localPageCSS is injected into every HTML page Kouki serves from localhost.
const localPageCSS = `
:root{color-scheme:dark}
html,body{background:#14161a !important;color:#e6e8ec !important}
body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif !important}
h1,h2,h3,h4{color:#f5f7fa !important}
a{color:#7aa2f7 !important}
small,label{color:#9aa3b2 !important}
hr{border-color:#2b2f36 !important}
input,select,textarea{
  background:#1c1f25 !important;color:#e6e8ec !important;
  border:1px solid #333944 !important;border-radius:8px !important;
  padding:10px 12px !important;width:100% !important;box-sizing:border-box !important;
}
input::placeholder{color:#6b7280 !important}
input:focus,select:focus,textarea:focus{outline:2px solid #3b82f6 !important;outline-offset:1px !important}
button{
  background:#2563eb !important;color:#ffffff !important;border:0 !important;
  border-radius:8px !important;padding:10px 16px !important;font-size:15px !important;
  cursor:pointer !important;margin-right:8px !important;
}
button:hover{background:#1d4ed8 !important}
div[style*="border"],div[style*="border-radius"]{background:#1c1f25 !important;border-color:#2b2f36 !important}
table{background:#1c1f25 !important;color:#e6e8ec !important}
tr:nth-child(even){background:#20242b !important}
code,pre{background:#0f1115 !important;color:#c9d1d9 !important}
`

// darkWriter buffers an HTML response so the stylesheet can be injected before
// the closing head tag. These pages are a few kilobytes, so buffering is free
// and lets the rewrite happen before any byte reaches the browser.
type darkWriter struct {
	http.ResponseWriter
	buf  bytes.Buffer
	code int
}

func (w *darkWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

// Write buffers the response instead of forwarding it, retaining net/http's
// implicit 200 status if a handler writes before calling WriteHeader.
func (w *darkWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(p)
}

// Flush satisfies http.Flusher, which net/http type-asserts for. There is
// nothing to flush while buffering.
func (w *darkWriter) Flush() {}

func (w *darkWriter) finish() {
	body := w.buf.Bytes()
	contentType := w.ResponseWriter.Header().Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		// HTML tag names are case-insensitive. Only rewrite HTML: JSON API
		// responses may legitimately contain the text "</head>" in a value.
		if i := bytes.Index(bytes.ToLower(body), []byte("</head>")); i >= 0 {
			style := []byte("<style>" + localPageCSS + "</style>")
			merged := make([]byte, 0, len(body)+len(style))
			merged = append(merged, body[:i]...)
			merged = append(merged, style...)
			merged = append(merged, body[i:]...)
			body = merged
			// A length set by the original handler describes the unmodified
			// body. Let net/http compute the new length after injection.
			w.ResponseWriter.Header().Del("Content-Length")
		}
	}
	code := w.code
	if code == 0 {
		code = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(code)
	_, _ = w.ResponseWriter.Write(body)
}

// withDarkPage wraps an http.Handler so every HTML response it produces carries
// the dark stylesheet.
func withDarkPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := &darkWriter{ResponseWriter: w}
		next.ServeHTTP(dw, r)
		dw.finish()
	})
}

// ---------------------------------------------------------------------------
// native window
// ---------------------------------------------------------------------------

var (
	uxtheme                           = windows.NewLazySystemDLL("uxtheme.dll")
	procSetPreferredAppMode           = uxtheme.NewProc("SetPreferredAppMode")
	procAllowDarkModeForWindow        = uxtheme.NewProc("AllowDarkModeForWindow")
	procRefreshImmersiveColorPolicySt = uxtheme.NewProc("RefreshImmersiveColorPolicyState")
)

// preferredAppModeAllowDark is the undocumented value 2. Windows 10 1809
// shipped only 0 (legacy) and 1 (force light); later builds honour 2.
const preferredAppModeAllowDark = 2

var darkAppModeSet bool

// enableDarkModeForWindow opts one HWND into Windows' immersive dark mode.
//
// The uxtheme entry points are undocumented and absent on very old builds, so
// every call is guarded with Find(); when they are missing the window simply
// keeps the normal theme.
func enableDarkModeForWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}

	if !darkAppModeSet {
		if procSetPreferredAppMode.Find() == nil {
			_, _, _ = procSetPreferredAppMode.Call(uintptr(preferredAppModeAllowDark))
		}
		darkAppModeSet = true
	}
	if procAllowDarkModeForWindow.Find() == nil {
		_, _, _ = procAllowDarkModeForWindow.Call(hwnd)
	}
	if procRefreshImmersiveColorPolicySt.Find() == nil {
		_, _, _ = procRefreshImmersiveColorPolicySt.Call()
	}

	// Title bar and borders. Attribute 20 is DWMWA_USE_IMMERSIVE_DARK_MODE on
	// Win10 2004+; 19 was the same flag on 1809/1909.
	//
	// DwmSetWindowAttribute takes windows.HWND (a uintptr-based type), which is
	// why the window handle is passed as a plain uintptr all the way through.
	handle := windows.HWND(hwnd)
	on := int32(1)
	for _, attr := range []uint32{20, 19} {
		_ = windows.DwmSetWindowAttribute(handle, attr, ptr32(&on), 4)
	}
}

// darkSurface is painted behind Kouki's canvas. It must match ui.go's
// cBackground exactly, otherwise the window flashes a second colour during the
// first frame and any repaint that misses a pixel shows through.
var darkSurface = walk.RGB(0x11, 0x13, 0x18)

// applyDarkChrome makes the main window dark. It runs before Show() so Windows
// knows about the preference before the first paint.
func applyDarkChrome(mw *walk.MainWindow) {
	if mw == nil {
		return
	}
	if brush, err := walk.NewSolidColorBrush(darkSurface); err == nil {
		mw.SetBackground(brush)
	}
	enableDarkModeForWindow(uintptr(mw.Handle()))
}

// ---------------------------------------------------------------------------
// usable desktop area
// ---------------------------------------------------------------------------

// procSystemParametersInfoW is loaded straight from the DLL handle, without a
// package-level one of its own: main.go already declares a `user32` lazy DLL for
// MessageBoxW, and a second one under the same name would not compile.
var procSystemParametersInfoW = windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")

// spiGetWorkArea is SystemParametersInfo's SPI_GETWORKAREA action: the desktop
// rectangle left over once the taskbar and the notification area are removed.
const spiGetWorkArea = 0x0030

// workArea returns that rectangle, so the window can be sized to the screen it
// is actually on. The fallback is a conventional 1080p desktop; it exists only
// for the theoretical case of the call being unavailable, which every supported
// Windows build rules out.
func workArea() walk.Rectangle {
	var r windows.Rect
	if procSystemParametersInfoW.Find() == nil {
		ret, _, _ := procSystemParametersInfoW.Call(
			uintptr(spiGetWorkArea), 0, uintptr(unsafe.Pointer(&r)), 0)
		if ret != 0 && r.Right > r.Left && r.Bottom > r.Top {
			return walk.Rectangle{
				X:      int(r.Left),
				Y:      int(r.Top),
				Width:  int(r.Right - r.Left),
				Height: int(r.Bottom - r.Top),
			}
		}
	}
	return walk.Rectangle{X: 0, Y: 0, Width: 1920, Height: 1040}
}
