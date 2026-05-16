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
	Cfg        *config.Control
	Store      *store.Store
	Pay        payment.Payment
	PaySvc     *payment.Service
	Orch       *orchestrator.Service
	APIKeys    *auth.APIKeyCache
	EventBus   *mcp.SandboxEventBus
	FileLogger func(http.Handler) http.Handler
}

// NewRouter builds the HTTP mux.
func NewRouter(d RouterDeps) http.Handler {
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
	rl := auth.NewRateLimiter(d.Cfg.MCPRPM)

	mcpSvc := mcp.NewService(d.Cfg, d.Store, d.PaySvc, d.Orch, nil)
	mcpSvc.EventBus = d.EventBus
	mcpSvc.Sessions = mcp.NewSessionRegistry(d.Cfg.MCPSessionTTL, d.Cfg.MCPSessionEventBuf)
	mcpSvc.Sessions.StartCleanupLoop(0)
	mcpSvc.Executions = mcp.NewExecutionRegistry()
	if d.EventBus != nil {
		d.EventBus.SetSessionRegistry(mcpSvc.Sessions)
	}

	postMCPURL := controlPlaneOrigin(d) + "/v1/mcp"
	mux.Handle("GET /v1/mcp/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mcp.HealthPayload())
	}))
	// GET /v1/mcp: if Mcp-Session-Id is present, serve session SSE stream (Streamable HTTP spec);
	// otherwise fall back to legacy SSE handshake that advertises the POST endpoint.
	streamH := mcp.AuthMiddleware(d.APIKeys, d.Store, rl.Middleware(mcp.StreamHandler(mcpSvc.Sessions)))
	legacySSE := rl.Middleware(mcp.SSEHandler(postMCPURL))
	mux.Handle("GET /v1/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") != "" || r.URL.Query().Get("session_id") != "" {
			streamH.ServeHTTP(w, r)
			return
		}
		legacySSE.ServeHTTP(w, r)
	}))
	mux.Handle("POST /v1/mcp", mcp.AuthMiddleware(d.APIKeys, d.Store, rl.Middleware(mcp.Handler(mcpSvc))))
	// Explicit /v1/mcp/stream path kept for backward compatibility with clients using the notifications hint.
	mux.Handle("GET /v1/mcp/stream", streamH)
	// Legacy per-sandbox SSE billing notifications (kept for backward compatibility).
	if d.EventBus != nil {
		mux.Handle("GET /v1/mcp/notifications", mcp.AuthMiddleware(d.APIKeys, d.Store, rl.Middleware(mcp.NotificationsHandler(mcpSvc, d.EventBus))))
	}

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

	// Sandbox lifecycle — API key (credits) only on REST. Lightning sandboxes are managed via MCP.
	apiKey := func(h http.Handler) http.Handler { return rl.Middleware(d.APIKeys.Middleware(h)) }
	mux.Handle("POST /v1/sandbox/create", apiKey(http.HandlerFunc(handleCreateSandbox(d))))
	mux.Handle("GET /v1/sandbox/{id}", apiKey(http.HandlerFunc(handleGetSandbox(d))))
	mux.Handle("POST /v1/sandbox/{id}/pause", apiKey(http.HandlerFunc(handlePauseSandbox(d))))
	mux.Handle("POST /v1/sandbox/{id}/resume", apiKey(http.HandlerFunc(handleResumeSandbox(d))))
	mux.Handle("DELETE /v1/sandbox/{id}", apiKey(http.HandlerFunc(handleDeleteSandbox(d))))
	mux.Handle("POST /v1/sandbox/{id}/refresh-data-jwt", apiKey(http.HandlerFunc(handleRefreshDataJWT(d))))
	mux.Handle("POST /v1/sandbox/{id}/extend-balance", apiKey(http.HandlerFunc(handleExtendBalance(d))))
	mux.Handle("GET /v1/sandbox/{id}/billing", apiKey(http.HandlerFunc(handleSandboxBilling(d))))

	// Runtime callbacks
	mux.Handle("POST /v1/runtime/billing/audit-batch", auth.BearerSecret(d.Cfg.CallbackSecret, http.HandlerFunc(handleAuditBatch(d))))
	mux.Handle("POST /v1/runtime/topup-trigger", auth.BearerSecret(d.Cfg.CallbackSecret, http.HandlerFunc(handleTopupTrigger(d))))
	mux.Handle("POST /v1/runtime/pause-trigger", auth.BearerSecret(d.Cfg.CallbackSecret, http.HandlerFunc(handlePauseTrigger(d))))

	// Stripe
	mux.Handle("POST /internal/webhooks/stripe", http.HandlerFunc(handleStripeWebhook(d)))

	handler := requestLogger(recoverReqIDLog(mux))
	if d.FileLogger != nil {
		return d.FileLogger(handler)
	}
	return handler
}

func controlPlaneOrigin(d RouterDeps) string {
	base := strings.TrimRight(strings.TrimSpace(d.Cfg.ControlPlanePublicURL), "/")
	if base == "" {
		base = "http://127.0.0.1:" + d.Cfg.Port
	}
	return base
}
