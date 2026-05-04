package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/config"
)

func TestHealthPayload(t *testing.T) {
	h := HealthPayload()
	if h["status"] != "ok" || h["protocol_version"] != protocolVersionDefault {
		t.Fatal(h)
	}
}

func TestToolListCount(t *testing.T) {
	if n := len(toolList()); n != 13 {
		t.Fatalf("expected 13 tools, got %d", n)
	}
}

func TestHandlerInitializeAndToolsList(t *testing.T) {
	cfg := &config.Control{Port: "8080", MCPRequestBodyLimit: 1 << 20}
	svc := NewService(cfg, nil, nil, nil, nil)
	h := Handler(svc)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	res, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Mcp-Session-Id") == "" {
		t.Fatal("missing Mcp-Session-Id")
	}
	var env Response
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error != nil {
		t.Fatal(env.Error)
	}
	m, _ := env.Result.(map[string]any)
	if m["protocolVersion"] != protocolVersion2025 {
		t.Fatal(m)
	}

	body2 := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	res2, err := http.Post(srv.URL, "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var env2 Response
	if err := json.NewDecoder(res2.Body).Decode(&env2); err != nil {
		t.Fatal(err)
	}
	if env2.Error != nil {
		t.Fatal(env2.Error)
	}
	rm, _ := env2.Result.(map[string]any)
	tools, _ := rm["tools"].([]any)
	if len(tools) != 13 {
		t.Fatalf("tools/list: got %d tools", len(tools))
	}
}

func TestHandlerInitializeNegotiates202506(t *testing.T) {
	cfg := &config.Control{Port: "8080", MCPRequestBodyLimit: 1 << 20}
	svc := NewService(cfg, nil, nil, nil, nil)
	h := Handler(svc)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	res, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env Response
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error != nil {
		t.Fatal(env.Error)
	}
	m, _ := env.Result.(map[string]any)
	if m["protocolVersion"] != protocolVersion202506 {
		t.Fatal(m)
	}
}

func TestHandlerInitializeNegotiates2024(t *testing.T) {
	cfg := &config.Control{Port: "8080", MCPRequestBodyLimit: 1 << 20}
	svc := NewService(cfg, nil, nil, nil, nil)
	h := Handler(svc)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	res, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env Response
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error != nil {
		t.Fatal(env.Error)
	}
	m, _ := env.Result.(map[string]any)
	if m["protocolVersion"] != protocolVersion2024 {
		t.Fatal(m)
	}
}

func TestHandlerUnknownTool(t *testing.T) {
	cfg := &config.Control{Port: "8080"}
	svc := NewService(cfg, nil, nil, nil, nil)
	h := Handler(svc)
	id := uuid.New()
	p := &auth.Principal{Kind: auth.KindL402, AgentID: &id, BalanceKey: "test"}
	body := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"not_a_real_tool","arguments":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var env Response
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		t.Fatal("expected rpc error")
	}
}

func TestCallToolUnknownName(t *testing.T) {
	svc := NewService(&config.Control{}, nil, nil, nil, nil)
	res, st, slug, _ := svc.CallTool(t.Context(), nil, "bogus_tool", []byte(`{}`))
	if res != nil || st != 0 || slug != "" {
		t.Fatalf("got res=%v st=%d slug=%q", res, st, slug)
	}
}
