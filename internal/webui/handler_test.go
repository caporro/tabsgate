package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestEmbeddedAssets(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatalf("UI request reached API: %s", r.URL.Path) })
	handler := Handler(api)
	for path, contentType := range map[string]string{
		"/": "text/html", "/assets/app.css": "text/css", "/assets/app.js": "text/javascript",
		"/assets/api.js": "text/javascript", "/assets/confirm.js": "text/javascript", "/assets/icon.svg": "image/svg+xml",
		"/assets/logo.png": "image/png",
	} {
		t.Run(path, func(t *testing.T) {
			get := httptest.NewRecorder()
			handler.ServeHTTP(get, httptest.NewRequest("GET", path, nil))
			if get.Code != 200 || !strings.HasPrefix(get.Header().Get("Content-Type"), contentType) || get.Body.Len() == 0 {
				t.Fatalf("asset response: %d %v", get.Code, get.Header())
			}
			if get.Header().Get("Set-Cookie") != "" || get.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("UI must not set cookies or cache sensitive page state")
			}
			if !strings.Contains(get.Header().Get("Content-Security-Policy"), "script-src 'self'") || get.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("missing browser protections")
			}
			if get.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
				t.Fatal("incorrect asset length")
			}
			head := httptest.NewRecorder()
			handler.ServeHTTP(head, httptest.NewRequest("HEAD", path, nil))
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
				t.Fatal("HEAD response differs from GET metadata")
			}
		})
	}
}

func TestAPIDelegation(t *testing.T) {
	for _, path := range []string{"/api/v1/providers", "/api/v1/connections", "/v1/chat/completions", "/healthz", "/readyz", "/missing", "/assets/private.txt"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest("POST", path, strings.NewReader(`{"test":true}`))
			request.Header.Set("Authorization", "Bearer test-credential")
			request.Header.Set("X-TabsGate-Keyword", "test-pass")
			called := false
			handler := Handler(http.HandlerFunc(func(w http.ResponseWriter, got *http.Request) {
				called = true
				body, _ := io.ReadAll(got.Body)
				if got != request || string(body) != `{"test":true}` || got.Header.Get("Authorization") != "Bearer test-credential" || got.Header.Get("X-TabsGate-Keyword") != "test-pass" {
					t.Fatal("UI wrapper changed an API request")
				}
				w.Header().Set("X-API", "unchanged")
				w.WriteHeader(http.StatusAccepted)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if !called || response.Code != 202 || response.Header().Get("X-API") != "unchanged" {
				t.Fatal("API response was not preserved")
			}
		})
	}
}

func TestUIRejectsQueryCredentials(t *testing.T) {
	handler := Handler(http.NotFoundHandler())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/?key=test-secret", nil))
	if response.Code != 400 || strings.Contains(response.Body.String(), "test-secret") {
		t.Fatal("query parameters must not be accepted or reflected")
	}
}
