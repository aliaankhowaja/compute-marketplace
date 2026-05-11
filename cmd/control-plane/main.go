// Command control-plane runs sockt-cp on Cloud Run.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/aliaankhowaja/sockt-backend/internal/api"
	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/orchestrator"
	"github.com/aliaankhowaja/sockt-backend/internal/payment"
	"github.com/aliaankhowaja/sockt-backend/internal/payment/lnbot"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	logLevel := strings.ToLower(cfg.LogLevel)
	if logLevel == "debug" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if cfg.UsePodMock {
		slog.Info("pod_mock_enabled", "url", cfg.PodMockURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	var lnBot *lnbot.Client
	if strings.TrimSpace(cfg.LNBOTAPIKey) != "" {
		lnBot = lnbot.New(cfg.LNBOTAPIBaseURL, cfg.LNBOTAPIKey, cfg.LNBOTWalletID, cfg.LNBOTHTTPTimeout)
	} else {
		slog.Info("lnbot_disabled")
	}

	net := &chaincfg.MainNetParams
	if strings.EqualFold(cfg.LightningNetwork, "testnet") {
		net = &chaincfg.TestNet3Params
	}

	var paySvc *payment.Service
	var payIface payment.Payment
	if lnBot != nil {
		paySvc = payment.NewService(lnBot, nil, st)
		paySvc.Chain = net
		payIface = paySvc
	}

	keyCache, err := auth.NewAPIKeyCache(st, cfg.APIKeyCacheSize)
	if err != nil {
		slog.Error("api_key_cache", "err", err)
		os.Exit(1)
	}
	auth.StartRevokedListener(ctx, cfg.SupabaseDBSessionDSN, keyCache)

	orch := &orchestrator.Service{
		Client: orchestrator.NewRunPodClient(cfg),
		Store:  st,
		Cfg:    cfg,
	}

	if lnBot != nil {
		deps := &payment.DepositSettlerBackground{
			Store:            st,
			LNBot:            lnBot,
			Interval:         15 * time.Second,
			OnSandboxSettled: orchestrator.SandboxDepositObserver(orch, st, cfg),
		}
		go deps.Run(ctx)
	}

	handler := api.NewRouter(api.RouterDeps{
		Cfg:     cfg,
		Store:   st,
		Pay:     payIface,
		PaySvc:  paySvc,
		Orch:    orch,
		APIKeys: keyCache,
	})

	addr := cfg.ListenHTTP
	if addr == "" {
		addr = ":" + cfg.Port
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
