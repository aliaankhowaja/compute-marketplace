package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	port := os.Getenv("POD_MOCK_PORT")
	if port == "" {
		port = "9090"
	}

	dm, err := NewDockerManager()
	if err != nil {
		slog.Error("docker init", "err", err)
		os.Exit(1)
	}

	proxyHost := os.Getenv("POD_MOCK_PROXY_HOST")
	if proxyHost == "" {
		proxyHost = "localhost"
	}

	st := NewStore()
	mux := NewRouter(st, dm, proxyHost)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("pod-mock listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down, cleaning up containers...")
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dm.Cleanup(cleanupCtx)
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = srv.Shutdown(shutdownCtx)
	slog.Info("pod-mock stopped")
}
