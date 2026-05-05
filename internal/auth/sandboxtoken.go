package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// SandboxTokenPrefix is the literal prefix every per-sandbox bearer token starts with.
const SandboxTokenPrefix = "sbx_"

// SandboxTokenLookup resolves an opaque sandbox token to its sandbox row.
// The returned ID is the sandbox UUID; an empty UUID with a nil error means "no such token".
type SandboxTokenLookup interface {
	LookupSandboxByToken(ctx context.Context, token string) (sandboxID uuid.UUID, found bool, err error)
}

// IsSandboxToken reports whether the bearer string looks like a sandbox token.
func IsSandboxToken(raw string) bool {
	return strings.HasPrefix(raw, SandboxTokenPrefix)
}

// SandboxTokenMiddleware authenticates `Authorization: Bearer sbx_…` tokens. Unknown
// tokens are rejected with 401; missing or non-sbx tokens fall through to next without
// a principal so callers can layer this in front of other middlewares (API key, etc.)
// or pass through anonymously to an MCP handler.
func SandboxTokenMiddleware(lookup SandboxTokenLookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r.Header.Get("Authorization"))
		if !IsSandboxToken(raw) {
			next.ServeHTTP(w, r)
			return
		}
		if lookup == nil {
			SocktBearerWWWAuthenticate(w)
			http.Error(w, "sandbox token lookup not configured", http.StatusServiceUnavailable)
			return
		}
		id, ok, err := lookup.LookupSandboxByToken(r.Context(), raw)
		if err != nil {
			http.Error(w, "sandbox token lookup error", http.StatusInternalServerError)
			return
		}
		if !ok {
			SocktBearerWWWAuthenticate(w)
			http.Error(w, "invalid sandbox token", http.StatusUnauthorized)
			return
		}
		sid := id
		p := &Principal{
			Kind:       KindSandbox,
			SandboxID:  &sid,
			BalanceKey: "sandbox:" + sid.String(),
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
