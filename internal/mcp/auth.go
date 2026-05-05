package mcp

import (
	"net/http"
	"strings"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

// AuthMiddleware routes POST /v1/mcp authentication based on the bearer prefix:
//
//   - `sockt_live_…` → API-key middleware (credits path).
//   - `sbx_…`        → sandbox-token middleware (per-sandbox lightning prepay).
//   - anything else  → pass through anonymously (the handler may serve unauthenticated
//     calls such as `initialize`, `tools/list`, or anonymous `sandbox_create`).
//
// JWT bearer tokens for the dashboard are intentionally not accepted on the MCP
// surface; the MCP path is reserved for autonomous agents (API key or sandbox token).
func AuthMiddleware(keys *auth.APIKeyCache, sandboxTokens auth.SandboxTokenLookup, next http.Handler) http.Handler {
	apiH := keys.Middleware(next)
	sbxH := auth.SandboxTokenMiddleware(sandboxTokens, next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := auth.BearerFromRequest(r)
		switch {
		case strings.HasPrefix(raw, "sockt_live_"):
			apiH.ServeHTTP(w, r)
		case auth.IsSandboxToken(raw):
			sbxH.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
