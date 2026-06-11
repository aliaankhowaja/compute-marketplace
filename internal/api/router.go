// Package api exposes the control-plane HTTP surface.
package api

import (
	"net/http"

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
	FileLogger func(http.Handler) http.Handler
}

// NewRouter builds the HTTP mux.
func NewRouter(d RouterDeps) http.Handler {
	mux := http.NewServeMux()

	// Cloud Run reserves some paths ending in "z"; /healthz never reaches the container.
	// See https://cloud.google.com/run/docs/known-issues#ah — use /health for probes on Run.
	handleCPHealth := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
	mux.HandleFunc("GET /healthz", handleCPHealth)
	mux.HandleFunc("GET /health", handleCPHealth)

	// OAuth/OIDC discovery is not supported; explicit 404 avoids ambiguous client behavior.
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", http.NotFound)
	mux.HandleFunc("HEAD /.well-known/oauth-authorization-server", http.NotFound)
	mux.HandleFunc("GET /.well-known/openid-configuration", http.NotFound)
	mux.HandleFunc("HEAD /.well-known/openid-configuration", http.NotFound)

	rl := auth.NewRateLimiter(d.Cfg.MCPRPM)

	mcpSvc := mcp.NewService(d.Cfg, d.Store, d.PaySvc, d.Orch, nil)
	mcpSvc.Sessions = mcp.NewSessionRegistry(d.Cfg.MCPSessionTTL)
	mcpSvc.Sessions.StartCleanupLoop(0)
	mcpSvc.Executions = mcp.NewExecutionRegistry()

	// MCP (JSON-RPC 2.0)
	mux.Handle("GET /v1/mcp/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mcp.HealthPayload())
	}))
	mux.Handle("POST /v1/mcp", mcp.AuthMiddleware(d.APIKeys, d.Store, rl.Middleware(mcp.Handler(mcpSvc))))

	// REST sandbox endpoints — same auth as MCP (API key, sandbox token, or anonymous)
	restAuth := func(h http.Handler) http.Handler {
		return mcp.AuthMiddleware(d.APIKeys, d.Store, rl.Middleware(h))
	}
	mux.Handle("GET /v1/sandboxes/tiers", restAuth(http.HandlerFunc(handleRESTListTiers(mcpSvc))))
	mux.Handle("POST /v1/sandboxes", restAuth(http.HandlerFunc(handleRESTCreateSandbox(mcpSvc))))
	mux.Handle("POST /v1/sandboxes/{id}/exec", restAuth(http.HandlerFunc(handleRESTExec(mcpSvc))))
	mux.Handle("POST /v1/sandboxes/{id}/files/write", restAuth(http.HandlerFunc(handleRESTWriteFile(mcpSvc))))
	mux.Handle("POST /v1/sandboxes/{id}/files/read", restAuth(http.HandlerFunc(handleRESTReadFile(mcpSvc))))
	mux.Handle("GET /v1/sandboxes/{id}/files", restAuth(http.HandlerFunc(handleRESTListFiles(mcpSvc))))
	mux.Handle("GET /v1/sandboxes/{id}", restAuth(http.HandlerFunc(handleRESTSandboxStatus(mcpSvc))))
	mux.Handle("POST /v1/sandboxes/{id}/pause", restAuth(http.HandlerFunc(handleRESTPause(mcpSvc))))
	mux.Handle("POST /v1/sandboxes/{id}/resume", restAuth(http.HandlerFunc(handleRESTResume(mcpSvc))))
	mux.Handle("DELETE /v1/sandboxes/{id}", restAuth(http.HandlerFunc(handleRESTTerminate(mcpSvc))))
	mux.Handle("GET /v1/executions/{execution_id}", restAuth(http.HandlerFunc(handleRESTExecResult(mcpSvc))))
	mux.Handle("DELETE /v1/executions/{execution_id}", restAuth(http.HandlerFunc(handleRESTExecCancel(mcpSvc))))

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
