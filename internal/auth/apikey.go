package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/golang-lru/v2"
	"github.com/jackc/pgx/v5"

	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// APIKeyCache resolves hashed API keys with an LRU fronting Postgres.
type APIKeyCache struct {
	St    *store.Store
	inner *lru.Cache[string, *store.APIKey]
	mu    sync.Mutex
}

// NewAPIKeyCache creates a resolver with the given capacity (0 = default 10k).
func NewAPIKeyCache(st *store.Store, capacity int) (*APIKeyCache, error) {
	if capacity <= 0 {
		capacity = 10_000
	}
	c, err := lru.New[string, *store.APIKey](capacity)
	if err != nil {
		return nil, err
	}
	return &APIKeyCache{St: st, inner: c}, nil
}

// Purge clears all cached keys (on revoke broadcast).
func (a *APIKeyCache) Purge() {
	if a == nil || a.inner == nil {
		return
	}
	a.mu.Lock()
	a.inner.Purge()
	a.mu.Unlock()
}

func (a *APIKeyCache) get(ctx context.Context, hash string) (*store.APIKey, error) {
	if a == nil || a.St == nil {
		return nil, nil
	}
	a.mu.Lock()
	v, ok := a.inner.Get(hash)
	a.mu.Unlock()
	if ok && v != nil {
		return v, nil
	}
	k, err := a.St.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if k != nil && k.IsActive {
		a.mu.Lock()
		a.inner.Add(hash, k)
		a.mu.Unlock()
	}
	return k, nil
}

// Middleware authenticates `Bearer sockt_live_...` API keys.
func (a *APIKeyCache) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r.Header.Get("Authorization"))
		if raw == "" || !strings.HasPrefix(raw, "sockt_live_") {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		hash := store.HashAPIKey(raw)
		k, err := a.get(r.Context(), hash)
		if err != nil || k == nil || !k.IsActive {
			http.Error(w, "invalid api key", http.StatusUnauthorized)
			return
		}
		uid := k.UserID
		p := &Principal{Kind: KindAPIKey, UserID: &uid, APIKeyID: &k.ID, BalanceKey: "apikey:" + k.ID.String()}
		go func(id uuid.UUID) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a.St.TouchAPIKeyUsed(ctx, id)
		}(k.ID)
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// StartRevokedListener LISTENs on apikey_revoked and purges the cache.
func StartRevokedListener(ctx context.Context, sessionDSN string, cache *APIKeyCache) {
	if sessionDSN == "" || cache == nil {
		return
	}
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			if err := listenOnce(ctx, sessionDSN, cache); err != nil {
				slog.Warn("apikey_revoked listener", "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
			}
		}
	}()
}

func listenOnce(ctx context.Context, dsn string, cache *APIKeyCache) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "LISTEN apikey_revoked"); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if n != nil {
			cache.Purge()
		}
	}
}
