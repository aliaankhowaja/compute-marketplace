package runtime

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// Scope constants for data_jwt enforcement.
const (
	ScopeExec  = "exec"
	ScopeFiles = "files"
	ScopeShell = "shell"
	ScopeAll   = "all"
)

// DataJWTClaims are pod-scoped JWT claims minted by the control plane.
type DataJWTClaims struct {
	SandboxID string `json:"sandbox_id"`
	PodID     string `json:"pod_id"`
	Scope     string `json:"scope"`
	jwt.RegisteredClaims
}

var (
	// ErrTokenInvalid is returned when the JWT cannot be parsed or verified.
	ErrTokenInvalid = errors.New("invalid data_jwt")
	// ErrTokenExpired is returned when exp is in the past.
	ErrTokenExpired = errors.New("data_jwt expired")
	// ErrSandboxMismatch is returned when sandbox_id does not match this pod.
	ErrSandboxMismatch = errors.New("sandbox_id mismatch")
	// ErrPodMismatch is returned when pod_id does not match this pod.
	ErrPodMismatch = errors.New("pod_id mismatch")
	// ErrJWTAudience is returned when aud does not match the configured audience.
	ErrJWTAudience = errors.New("invalid data_jwt audience")
	// ErrMissingScope is returned when the token lacks the required scope.
	ErrMissingScope = errors.New("missing scope")
)

// VerifyToken parses and validates data_jwt using HS256 and local pod metadata.
func VerifyToken(tokenStr string, cfg *Config, requiredScope string) (*DataJWTClaims, error) {
	tokenStr = strings.TrimSpace(tokenStr)
	if tokenStr == "" {
		return nil, ErrTokenInvalid
	}

	claims := &DataJWTClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok || t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return cfg.JWTSecret, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	if !token.Valid {
		return nil, ErrTokenInvalid
	}

	if cfg.JWTAudience != "" && !claims.VerifyAudience(cfg.JWTAudience, true) {
		return nil, ErrJWTAudience
	}

	if claims.SandboxID != cfg.SandboxID {
		return nil, ErrSandboxMismatch
	}
	if claims.PodID != cfg.PodID {
		return nil, ErrPodMismatch
	}

	if exp := claims.ExpiresAt; exp != nil && !exp.IsZero() && time.Now().After(exp.Time) {
		return nil, ErrTokenExpired
	}

	if !scopeAllows(claims.Scope, requiredScope) {
		return nil, ErrMissingScope
	}

	return claims, nil
}

func scopeAllows(tokenScope, required string) bool {
	tokenScope = strings.TrimSpace(strings.ToLower(tokenScope))
	required = strings.TrimSpace(strings.ToLower(required))
	if tokenScope == "" || required == "" {
		return false
	}
	if tokenScope == ScopeAll || tokenScope == "*" {
		return true
	}
	for _, part := range strings.FieldsFunc(tokenScope, func(r rune) bool {
		return r == ',' || r == ' ' || r == '|'
	}) {
		part = strings.TrimSpace(part)
		if part == required || part == ScopeAll || part == "*" {
			return true
		}
	}
	return false
}
