package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/time/rate"
)

// RateLimiter is an in-memory per-key rate limiter (default ~5 req/s ≈ 300 rpm).
type RateLimiter struct {
	mu     sync.Mutex
	lim    map[string]*rate.Limiter
	burst  int
	rEvery rate.Limit // events per second
}

// NewRateLimiter creates a limiter. rpm is requests per minute per BalanceKey.
func NewRateLimiter(rpm int) *RateLimiter {
	if rpm <= 0 {
		rpm = 300
	}
	every := rate.Limit(float64(rpm) / 60.0)
	return &RateLimiter{
		lim:    make(map[string]*rate.Limiter),
		burst:  max(10, rpm/60),
		rEvery: every,
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Middleware enforces rate limits using Principal.BalanceKey when present, falling back
// to per-client-IP keys for anonymous requests so a single noisy IP can't starve others.
func (r *RateLimiter) Middleware(next http.Handler) http.Handler {
	if r == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := PrincipalFrom(req.Context())
		var key string
		if p != nil && p.BalanceKey != "" {
			key = p.BalanceKey
		} else {
			key = "ip:" + ClientIP(req)
		}
		lim := r.get(key)
		if !lim.Allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *RateLimiter) get(key string) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.lim[key]
	if !ok {
		l = rate.NewLimiter(r.rEvery, r.burst)
		r.lim[key] = l
	}
	return l
}

// AllowKey reports whether one request is allowed for key without consuming in Middleware twice.
func (r *RateLimiter) AllowKey(key string) bool {
	if r == nil || key == "" {
		return true
	}
	return r.get(key).Allow()
}

// ClientIP returns a best-effort client address for per-IP rate limiting (X-Forwarded-For first hop, then X-Real-Ip, then RemoteAddr).
func ClientIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
