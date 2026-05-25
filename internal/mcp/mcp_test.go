package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

const sandboxToolCount = 12

func TestToolListCount(t *testing.T) {
	cfg := &config.Control{Port: "8080"}
	if n := len(toolList(NewService(cfg, nil, nil, nil, nil), nil)); n != sandboxToolCount {
		t.Fatalf("expected %d tools, got %d", sandboxToolCount, n)
	}
}

func TestHandlerInitializeAndToolsList(t *testing.T) {
	cfg := &config.Control{
		Port:                  "8080",
		MCPRequestBodyLimit:   1 << 20,
		ControlPlanePublicURL: "https://cp.example",
		SandboxPrepaySecs:     30,
	}
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
	inst, _ := m["instructions"].(string)
	if !strings.Contains(inst, "/home/sandbox") {
		t.Fatalf("initialize instructions missing default SANDBOX_ROOT: %q", inst)
	}
	if !strings.Contains(inst, "result.sockt") {
		t.Fatalf("initialize instructions should point at result.sockt: %q", inst)
	}
	sockt, _ := m["sockt"].(map[string]any)
	if sockt["auth_state"] != "unauthenticated" {
		t.Fatalf("sockt.auth_state: %#v", sockt["auth_state"])
	}
	if sockt["control_plane_public_url"] != "https://cp.example" {
		t.Fatalf("sockt.control_plane_public_url: %#v", sockt["control_plane_public_url"])
	}
	steps, _ := sockt["onboarding_steps"].([]any)
	if len(steps) < 3 {
		t.Fatalf("onboarding_steps: %#v", steps)
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
	if len(tools) != sandboxToolCount {
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

func TestHandlerUnknownToolWithSession(t *testing.T) {
	cfg := &config.Control{Port: "8080"}
	svc := NewService(cfg, nil, nil, nil, nil)
	svc.Sessions = NewSessionRegistry(0)

	id := uuid.New()
	p := &auth.Principal{Kind: auth.KindSandbox, SandboxID: &id, BalanceKey: "test"}
	svc.Sessions.Create("test-session-ut", p)

	h := Handler(svc)
	body := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"not_a_real_tool","arguments":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "test-session-ut")
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json, got %q", ct)
	}
	var env Response
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool")
	}
	if !strings.Contains(env.Error.Message, "unknown tool") {
		t.Fatalf("expected 'unknown tool' in error message, got: %s", env.Error.Message)
	}
}

func TestCallToolUnknownName(t *testing.T) {
	svc := NewService(&config.Control{}, nil, nil, nil, nil)
	res, st, slug, _ := svc.CallTool(t.Context(), nil, "bogus_tool", []byte(`{}`))
	if res != nil || st != 0 || slug != "" {
		t.Fatalf("got res=%v st=%d slug=%q", res, st, slug)
	}
}

func TestHandlerToolsCallReturnsJSON(t *testing.T) {
	cfg := &config.Control{Port: "8080", MCPRequestBodyLimit: 1 << 20}
	svc := NewService(cfg, nil, nil, nil, nil)
	svc.Sessions = NewSessionRegistry(0)

	id := uuid.New()
	p := &auth.Principal{Kind: auth.KindSandbox, SandboxID: &id, BalanceKey: "test"}
	svc.Sessions.Create("test-session-1", p)

	h := Handler(svc)
	body := []byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"sandbox_status","arguments":{"sandbox_id":"` + id.String() + `"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "test-session-1")
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json, got %q", ct)
	}
	var env Response
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error != nil {
		t.Fatalf("unexpected error: %v", env.Error)
	}
}

func TestHandlerToolsCallNoSession(t *testing.T) {
	cfg := &config.Control{Port: "8080", MCPRequestBodyLimit: 1 << 20}
	svc := NewService(cfg, nil, nil, nil, nil)
	svc.Sessions = NewSessionRegistry(0)

	h := Handler(svc)
	body := []byte(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"sandbox_status","arguments":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var env Response
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		t.Fatal("expected session_required error")
	}
}

func TestHandlerToolsCallInvalidSession(t *testing.T) {
	cfg := &config.Control{Port: "8080", MCPRequestBodyLimit: 1 << 20}
	svc := NewService(cfg, nil, nil, nil, nil)
	svc.Sessions = NewSessionRegistry(0)

	h := Handler(svc)
	body := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"sandbox_status","arguments":{}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "nonexistent-session")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var env Response
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		t.Fatal("expected invalid_session error")
	}
}

