// Package auth implements middleware for the control plane (JWT, API keys, L402, callbacks).
package auth

import (
	"context"

	"github.com/google/uuid"
)

// Kind identifies how the request was authenticated.
type Kind string

const (
	KindL402     Kind = "l402"
	KindAPIKey   Kind = "api_key"
	KindUserJWT  Kind = "user_jwt"
	KindCallback Kind = "callback"
	KindSandbox  Kind = "sandbox"
)

// Principal is stored in context after successful auth.
type Principal struct {
	Kind       Kind
	UserID     *uuid.UUID
	AgentID    *uuid.UUID
	APIKeyID   *uuid.UUID
	SandboxID  *uuid.UUID // present when authenticated by per-sandbox bearer token
	JWTSub     *uuid.UUID // Supabase sub (UUID string from JWT)
	Caveats    map[string]string
	BalanceKey string // stable key for rate limiting
}

type ctxKey int

const principalCtxKey ctxKey = 1

// WithPrincipal attaches a principal to the context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey, p)
}

// PrincipalFrom returns the principal or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalCtxKey).(*Principal)
	return p
}
