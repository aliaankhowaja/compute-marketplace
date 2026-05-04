package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

// JWTAuth verifies Supabase-style HS256 bearer JWTs. `sub` must be a UUID (user id).
func JWTAuth(secret string) func(http.Handler) http.Handler {
	sec := []byte(strings.TrimSpace(secret))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(sec) == 0 {
				http.Error(w, "jwt not configured", http.StatusInternalServerError)
				return
			}
			raw := bearerToken(r.Header.Get("Authorization"))
			if raw == "" {
				SocktBearerWWWAuthenticate(w)
				http.Error(w, "missing authorization", http.StatusUnauthorized)
				return
			}
			tok, err := jwt.Parse(raw, func(t *jwt.Token) (interface{}, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok || t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
					return nil, errors.New("unexpected alg")
				}
				return sec, nil
			})
			if err != nil || !tok.Valid {
				SocktBearerWWWAuthenticate(w)
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			claims, ok := tok.Claims.(jwt.MapClaims)
			if !ok {
				SocktBearerWWWAuthenticate(w)
				http.Error(w, "invalid claims", http.StatusUnauthorized)
				return
			}
			sub, _ := claims["sub"].(string)
			sub = strings.TrimSpace(sub)
			uid, err := uuid.Parse(sub)
			if err != nil {
				SocktBearerWWWAuthenticate(w)
				http.Error(w, "invalid sub", http.StatusUnauthorized)
				return
			}
			p := &Principal{Kind: KindUserJWT, UserID: &uid, JWTSub: &uid, BalanceKey: "user:" + uid.String()}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func bearerToken(hv string) string {
	hv = strings.TrimSpace(hv)
	if len(hv) < 8 || !strings.HasPrefix(strings.ToLower(hv), "bearer ") {
		return ""
	}
	return strings.TrimSpace(hv[7:])
}

// BearerFromRequest extracts raw bearer token value.
func BearerFromRequest(r *http.Request) string {
	return bearerToken(r.Header.Get("Authorization"))
}

// BearerSecret wraps a handler: Authorization Bearer must equal secret (runtime callbacks).
func BearerSecret(secret string, next http.Handler) http.Handler {
	sec := strings.TrimSpace(secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r.Header.Get("Authorization"))
		if sec == "" || got != sec {
			SocktBearerWWWAuthenticate(w)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p := &Principal{Kind: KindCallback, BalanceKey: "callback"}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
