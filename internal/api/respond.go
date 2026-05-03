package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, codeStr, msg string) {
	writeJSON(w, code, map[string]any{
		"code":    codeStr,
		"error":   codeStr,
		"message": msg,
	})
}

func recoverReqIDLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			reqID = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", reqID)
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "request_id", reqID, "panic", rec)
				writeErr(w, http.StatusInternalServerError, "internal_error", "panic")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
