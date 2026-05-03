package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

func TestHealthzThroughRouter(t *testing.T) {
	cfg := &config.Control{Port: "8080", SupabaseDBTxDSN: "postgres://x"}
	st := &store.Store{}
	h := NewRouter(RouterDeps{Cfg: cfg, Store: st}, nil, []byte("01234567890123456789012345678901"))
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatal(out)
	}
}
