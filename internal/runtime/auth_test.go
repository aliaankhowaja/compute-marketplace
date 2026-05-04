package runtime

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

func TestVerifyTokenAudience(t *testing.T) {
	sec := []byte("01234567890123456789012345678901")
	tok, err := auth.MintDataJWT(sec, "sb1", "pod1", "exec", time.Hour, auth.DataJWTAudience)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		SandboxID:   "sb1",
		PodID:       "pod1",
		JWTSecret:   sec,
		JWTAudience: auth.DataJWTAudience,
	}
	if _, err := VerifyToken(tok, cfg, ScopeExec); err != nil {
		t.Fatalf("valid token: %v", err)
	}

	bad := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sandbox_id": "sb1",
		"pod_id":     "pod1",
		"scope":      "exec",
		"aud":        "wrong",
		"exp":        time.Now().Add(time.Hour).Unix(),
		"nbf":        time.Now().Add(-time.Minute).Unix(),
		"iat":        time.Now().Unix(),
	})
	badStr, err := bad.SignedString(sec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyToken(badStr, cfg, ScopeExec); err == nil {
		t.Fatal("expected audience error")
	}
}
