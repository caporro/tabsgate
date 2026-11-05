package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tabsgate/internal/gate"
	"tabsgate/internal/webui"
)

func main() {
	if err := run(); err != nil {
		slog.Error("tabsgate stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "keygen":
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			fmt.Println(base64.StdEncoding.EncodeToString(b))
			return nil
		case "healthcheck":
			url := "http://127.0.0.1:8080/readyz"
			if len(os.Args) > 2 {
				url = os.Args[2]
			}
			client := &http.Client{Timeout: 2 * time.Second}
			res, err := client.Get(url)
			if err != nil {
				return errors.New("readiness check failed")
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			if res.StatusCode != 200 {
				return errors.New("service is not ready")
			}
			return nil
		default:
			return errors.New("usage: tabsgate [keygen|healthcheck [url]]")
		}
	}
	cfg, err := gate.ConfigFromEnv()
	if err != nil {
		return err
	}
	app, err := gate.New(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	server := &http.Server{Addr: cfg.Addr, Handler: webui.Handler(app), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: cfg.RequestTimeout + 15*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { slog.Info("tabsgate listening", "address", cfg.Addr); errs <- server.ListenAndServe() }()
	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout+5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
