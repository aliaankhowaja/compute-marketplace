package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// NewServer wires sandbox routes and health checks.
func NewServer(cfg *Config, meter *Meter, cb *Callbacks) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "sockt-runtime"})
	})

	wrap := func(h http.Handler) http.Handler {
		return requireSandbox(cfg, h)
	}

	mux.Handle("POST /sandbox/{id}/exec", wrap(ExecHandler(cfg)))
	mux.Handle("POST /sandbox/{id}/files", wrap(FilesWriteHandler(cfg)))
	mux.Handle("GET /sandbox/{id}/files", wrap(FilesReadHandler(cfg)))
	mux.Handle("GET /sandbox/{id}/files/list", wrap(FilesListHandler(cfg)))
	mux.Handle("GET /sandbox/{id}/status", wrap(StatusHandler(cfg, meter)))
	mux.Handle("GET /sandbox/{id}/shell", wrap(ShellHandler(cfg, meter, cb)))

	return mux
}

func requireSandbox(cfg *Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" || id != cfg.SandboxID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	const prefix = "Bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTokenExpired):
		http.Error(w, "data_jwt_expired", http.StatusUnauthorized)
	case errors.Is(err, ErrMissingScope):
		http.Error(w, ErrMissingScope.Error(), http.StatusForbidden)
	case errors.Is(err, ErrSandboxMismatch), errors.Is(err, ErrPodMismatch):
		http.Error(w, ErrTokenInvalid.Error(), http.StatusForbidden)
	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}
