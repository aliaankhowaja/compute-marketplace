// Package api exposes the control-plane HTTP surface.
package api

import (
	"net/http"
	"strings"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/mcp"
	"github.com/aliaankhowaja/sockt-backend/internal/orchestrator"
	"github.com/aliaankhowaja/sockt-backend/internal/payment"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// RouterDeps wires handlers.
type RouterDeps struct {
	Cfg     *config.Control
	Store   *store.Store
	Pay     payment.Payment
	PaySvc  *payment.Service
	Orch    *orchestrator.Service
	APIKeys *auth.APIKeyCache
}

// NewRouter builds the HTTP mux.
func NewRouter(d RouterDeps, mint auth.Bolt11Mint, l402Root []byte) http.Handler {
	mux := http.NewServeMux()

	// Cloud Run reserves some paths ending in "z"; /healthz never reaches the container.
	// See https://cloud.google.com/run/docs/known-issues#ah — use /health for probes on Run.
	handleCPHealth := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                  true,
			"lightning_wallet_ok": d.PaySvc != nil && d.PaySvc.HasLightning(),
			"supabase_ok":         d.Cfg != nil && strings.TrimSpace(d.Cfg.SupabaseDBTxDSN) != "",
			"runpod_configured":   d.Cfg != nil && strings.TrimSpace(d.Cfg.RunPodAPIKey) != "",
		})
	}
	mux.HandleFunc("GET /healthz", handleCPHealth)
	mux.HandleFunc("GET /health", handleCPHealth)

	// OAuth/OIDC discovery is not supported; explicit 404 avoids ambiguous client behavior.
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", http.NotFound)
	mux.HandleFunc("HEAD /.well-known/oauth-authorization-server", http.NotFound)
	mux.HandleFunc("GET /.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("HEAD /.well-known/openid-configuration", http.NotFound)

	// Public catalog
	mux.Handle("GET /v1/pricing", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.Store.ListTierPricing(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}))

	jwtAuth := auth.JWTAuth(d.Cfg.SupabaseJWTSecret)
	rl := auth.NewRateLimiter(300)

	mcpSvc := mcp.NewService(d.Cfg, d.Store, d.PaySvc, d.Orch, nil)
	postMCPURL := controlPlaneOrigin(d) + "/v1/mcp"
	mux.Handle("GET /v1/mcp/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mcp.HealthPayload())
	}))
	// Legacy SSE: public handshake; POST /v1/mcp still requires auth.
	mux.Handle("GET /v1/mcp", rl.Middleware(mcp.SSEHandler(postMCPURL)))
	mux.Handle("POST /v1/mcp", mcp.AuthMiddleware(d.APIKeys, d.Cfg.SupabaseJWTSecret, mint, l402Root, rl.Middleware(mcp.Handler(mcpSvc))))

	// User JWT routes
	jt := func(h http.Handler) http.Handler {
		return jwtAuth(rl.Middleware(h))
	}
	mux.Handle("GET /v1/user/profile", jt(http.HandlerFunc(handleUserProfile(d))))
	mux.Handle("GET /v1/user/credits", jt(http.HandlerFunc(handleUserCredits(d))))
	mux.Handle("GET /v1/user/credits/history", jt(http.HandlerFunc(handleUserCreditsHistory(d))))
	mux.Handle("POST /v1/user/api-keys", jt(http.HandlerFunc(handleCreateAPIKey(d))))
	mux.Handle("GET /v1/user/api-keys", jt(http.HandlerFunc(handleListAPIKeys(d))))
	mux.Handle("DELETE /v1/user/api-keys/{id}", jt(http.HandlerFunc(handleRevokeAPIKey(d))))

	// Wallet (JWT)
	mux.Handle("POST /v1/wallet/offer", jt(http.HandlerFunc(handleWalletOffer(d))))
	mux.Handle("GET /v1/wallet/balance", jt(http.HandlerFunc(handleWalletBalance(d))))
	mux.Handle("POST /v1/wallet/deposit", jt(http.HandlerFunc(handleWalletDeposit(d))))
	mux.Handle("GET /v1/wallet/deposits/{payment_hash}", jt(http.HandlerFunc(handleWalletDepositStatus(d))))

	// Sandbox lifecycle — API key (credits) or L402 (lightning)
	sandboxBase := eitherAPIKeyOrL402(d.APIKeys, mint, l402Root, rl)

	mux.Handle("POST /v1/sandbox/create", sandboxBase(http.HandlerFunc(handleCreateSandbox(d))))
	mux.Handle("GET /v1/sandbox/{id}", sandboxBase(http.HandlerFunc(handleGetSandbox(d))))
	mux.Handle("POST /v1/sandbox/{id}/pause", sandboxBase(http.HandlerFunc(handlePauseSandbox(d))))
	mux.Handle("POST /v1/sandbox/{id}/resume", sandboxBase(http.HandlerFunc(handleResumeSandbox(d))))
	mux.Handle("DELETE /v1/sandbox/{id}", sandboxBase(http.HandlerFunc(handleDeleteSandbox(d))))
	mux.Handle("POST /v1/sandbox/{id}/refresh-data-jwt", sandboxBase(http.HandlerFunc(handleRefreshDataJWT(d))))
	mux.Handle("POST /v1/sandbox/{id}/extend-balance", sandboxBase(http.HandlerFunc(handleExtendBalance(d))))
	mux.Handle("GET /v1/sandbox/{id}/billing", sandboxBase(http.HandlerFunc(handleSandboxBilling(d))))

	// Runtime callbacks
	mux.Handle("POST /v1/runtime/billing/audit-batch", auth.BearerSecret(d.Cfg.CallbackSecret, http.HandlerFunc(handleAuditBatch(d))))
	mux.Handle("POST /v1/runtime/topup-trigger", auth.BearerSecret(d.Cfg.CallbackSecret, http.HandlerFunc(handleTopupTrigger(d))))
	mux.Handle("POST /v1/runtime/pause-trigger", auth.BearerSecret(d.Cfg.CallbackSecret, http.HandlerFunc(handlePauseTrigger(d))))

	// Stripe
	mux.Handle("POST /internal/webhooks/stripe", http.HandlerFunc(handleStripeWebhook(d)))

	return recoverReqIDLog(mux)
}

func controlPlaneOrigin(d RouterDeps) string {
	base := strings.TrimRight(strings.TrimSpace(d.Cfg.ControlPlanePublicURL), "/")
	if base == "" {
		base = "http://127.0.0.1:" + d.Cfg.Port
	}
	return base
}

func eitherAPIKeyOrL402(keys *auth.APIKeyCache, mint auth.Bolt11Mint, root []byte, rl *auth.RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		apih := keys.Middleware(next)
		l402h := auth.L402Middleware(mint, root, next)
		return rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := auth.BearerFromRequest(r)
			if strings.HasPrefix(raw, "sockt_live_") {
				apih.ServeHTTP(w, r)
				return
			}
			l402h.ServeHTTP(w, r)
		}))
	}
}
