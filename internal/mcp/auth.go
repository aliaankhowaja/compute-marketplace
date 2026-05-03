package mcp

import (
	"net/http"
	"strings"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

// AuthMiddleware tries API key, L402, then Supabase JWT (in that order).
func AuthMiddleware(keys *auth.APIKeyCache, jwtSecret string, mint auth.Bolt11Mint, l402Root []byte, next http.Handler) http.Handler {
	jwtH := auth.JWTAuth(jwtSecret)(next)
	l402H := auth.L402Middleware(mint, l402Root, next)
	apiH := keys.Middleware(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := auth.BearerFromRequest(r)
		authz := strings.TrimSpace(r.Header.Get("Authorization"))

		switch {
		case strings.HasPrefix(raw, "sockt_live_"):
			apiH.ServeHTTP(w, r)
		case strings.HasPrefix(strings.ToUpper(authz), "L402 "):
			l402H.ServeHTTP(w, r)
		default:
			jwtH.ServeHTTP(w, r)
		}
	})
}
