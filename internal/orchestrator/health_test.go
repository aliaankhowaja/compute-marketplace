package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForDataPlaneHealth_successAfterDelay(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "sockt-runtime"})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	err := WaitForDataPlaneHealth(ctx, srv.URL, client, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if attempts.Load() < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", attempts.Load())
	}
}

func TestWaitForDataPlaneHealth_timeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	client := &http.Client{Timeout: 50 * time.Millisecond}
	err := WaitForDataPlaneHealth(ctx, srv.URL, client, 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWaitForDataPlaneHealth_emptyURL(t *testing.T) {
	t.Parallel()
	err := WaitForDataPlaneHealth(context.Background(), "", &http.Client{Timeout: time.Second}, time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
}
