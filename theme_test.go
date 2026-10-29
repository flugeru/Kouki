//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestWithDarkPageOnlyRewritesHTML(t *testing.T) {
	const body = `{"value":"</head>"}`
	rr := httptest.NewRecorder()
	withDarkPage(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api", nil))
	if rr.Body.String() != body {
		t.Fatalf("JSON response was changed: %q", rr.Body.String())
	}
}

func TestWithDarkPageAdjustsHTMLLength(t *testing.T) {
	const body = "<!doctype html><HTML><HEAD><title>Kouki</title></HEAD><body>OK</body></HTML>"
	rr := httptest.NewRecorder()
	withDarkPage(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(body))
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<style>"+localPageCSS+"</style></HEAD>") {
		t.Fatal("dark stylesheet was not inserted before uppercase closing head")
	}
	if rr.Header().Get("Content-Length") != "" {
		t.Fatal("stale Content-Length remained after stylesheet injection")
	}
}
