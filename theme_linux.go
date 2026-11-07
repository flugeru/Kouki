package main

import (
	"bytes"
	"net/http"
)

const localPageCSS = `
:root{color-scheme:dark}
html,body{background:#10141c !important;color:#e8edf5 !important}
body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif !important}
h1,h2,h3,h4{color:#f7f9fc !important}
a{color:#8bb9ff !important}
small,label{color:#a8b6ca !important}
input,select,textarea{background:#1b2431 !important;color:#e8edf5 !important;border:1px solid #344357 !important;border-radius:10px !important;padding:10px 12px !important;box-sizing:border-box !important}
button{background:#298dca !important;color:white !important;border:0 !important;border-radius:10px !important;padding:10px 16px !important;cursor:pointer !important}
button:hover{background:#43b3eb !important}
`

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
func (w *darkWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *darkWriter) Flush()                      {}
func (w *darkWriter) finish() {
	body := w.buf.Bytes()
	if i := bytes.Index(body, []byte("</head>")); i >= 0 {
		style := []byte("<style>" + localPageCSS + "</style>")
		merged := make([]byte, 0, len(body)+len(style))
		merged = append(merged, body[:i]...)
		merged = append(merged, style...)
		body = append(merged, body[i:]...)
	}
	if w.code == 0 {
		w.code = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.code)
	_, _ = w.ResponseWriter.Write(body)
}

func withDarkPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := &darkWriter{ResponseWriter: w}
		next.ServeHTTP(dw, r)
		dw.finish()
	})
}
