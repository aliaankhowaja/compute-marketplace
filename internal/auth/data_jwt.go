package auth

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// DataJWTAudience is the JWT "aud" claim for data-plane tokens consumed by sockt-runtime.
const DataJWTAudience = "sockt-runtime"

// MintDataJWT returns an HS256 token compatible with runtime.DataJWTClaims.
// audience is copied into the JWT "aud" claim; empty string defaults to DataJWTAudience.
func MintDataJWT(secret []byte, sandboxID, podID, scope string, ttl time.Duration, audience string) (string, error) {
	if strings.TrimSpace(audience) == "" {
		audience = DataJWTAudience
	}
	if len(secret) == 0 {
		return "", fmt.Errorf("missing secret")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	podID = strings.TrimSpace(podID)
	if sandboxID == "" || podID == "" {
		return "", fmt.Errorf("sandbox and pod required")
	}
	if scope == "" {
		scope = "all"
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sandbox_id": sandboxID,
		"pod_id":     podID,
		"scope":      scope,
		"aud":        audience,
		"iat":        now.Unix(),
		"nbf":        now.Unix(),
		"exp":        now.Add(ttl).Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(secret)
}
