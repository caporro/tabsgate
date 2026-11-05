//go:build ignore

// Isolated server for UI integration checks. Explicitly built by ui_smoke.mjs;
// excluded from normal builds. All provider traffic stays on loopback.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tabsgate/internal/gate"
	"tabsgate/internal/webui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	directory, err := os.MkdirTemp("", "tabsgate-ui-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer ui-test-provider-key" && r.Header.Get("Authorization") != "Bearer ui-test-provider-key-rotated" {
			w.WriteHeader(401)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"},{"id":"other-model"}]}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/chat/completions" {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":40,"completion_tokens":35,"total_tokens":75}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()
	cfg := gate.DefaultConfig()
	cfg.DBPath = filepath.Join(directory, "ui.db")
	cfg.MasterKey = make([]byte, 32)
	if _, err := rand.Read(cfg.MasterKey); err != nil {
		return err
	}
	cfg.AllowPrivateUpstreams = true
	cfg.RequestTimeout = 5 * time.Second
	cfg.Providers = []gate.Provider{
		{ID: "test", Name: "Test provider (simulated)", BaseURL: upstream.URL + "/v1", OutputLimitField: "max_tokens"},
		{ID: "other", Name: "Other provider (simulated)", BaseURL: upstream.URL + "/v1", OutputLimitField: "max_tokens"},
	}
	app, err := gate.New(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: webui.Handler(app), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String()})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
