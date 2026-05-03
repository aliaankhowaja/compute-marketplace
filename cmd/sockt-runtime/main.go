package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aliaankhowaja/sockt-backend/internal/runtime"
)

func main() {
	cfg, err := runtime.Load()
	if err != nil {
		log.Fatalf("sockt-runtime: config: %v", err)
	}

	cb := runtime.NewCallbacks(cfg)
	meter := runtime.NewMeter(cfg, cb)
	handler := runtime.NewServer(cfg, meter, cb)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		meter.Start(ctx)
	}()

	go func() {
		var err error
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			log.Printf("sockt-runtime: listening https %s", cfg.ListenAddr)
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			log.Printf("sockt-runtime: listening http %s", cfg.ListenAddr)
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("sockt-runtime: server: %v", err)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("sockt-runtime: shutdown: %v", err)
	}

	_ = os.Stderr.Sync()
}
