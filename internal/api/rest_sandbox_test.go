package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

func testRouter(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	cfg := &config.Control{Port: "8080", SupabaseDBTxDSN: "postgres://x"}
	st := &store.Store{}
	h := NewRouter(RouterDeps{Cfg: cfg, Store: st})
	srv := httptest.NewServer(h)
	return srv, srv.Close
}

func getJSON(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func doReq(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// TestRESTListTiers verifies the GET /v1/sandboxes/tiers route is wired.
// Without a real DB, the store panics on nil pool — recover middleware catches it.
// Either store_error or internal_error proves the route reached the handler.
func TestRESTListTiers(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := getJSON(t, srv, "/v1/sandboxes/tiers")
	if code != 500 {
		t.Fatalf("expected 500, got %d: %v", code, body)
	}
	c, _ := body["code"].(string)
	if c != "store_error" && c != "internal_error" {
		t.Fatalf("expected store_error or internal_error, got %v", body)
	}
}

// TestRESTCreateSandboxNoAuth verifies POST /v1/sandboxes reaches the handler anonymously.
// sandbox_create with no tier should return 400 invalid_params.
func TestRESTCreateSandboxNoAuth(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := doReq(t, srv, "POST", "/v1/sandboxes", map[string]any{
		"billing_method": "lightning",
	})
	// tier is required — handler validated it
	if code != 400 {
		t.Fatalf("expected 400, got %d: %v", code, body)
	}
	if body["code"] != "invalid_params" {
		t.Fatalf("expected invalid_params, got %v", body)
	}
}

// TestRESTCreateSandboxBadTier verifies the create flow reaches tier validation.
func TestRESTCreateSandboxBadTier(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := doReq(t, srv, "POST", "/v1/sandboxes", map[string]any{
		"billing_method": "lightning",
		"tier":           "nonexistent",
	})
	// store lookup for tier fails with nil pool
	if code != 500 {
		t.Fatalf("expected 500, got %d: %v", code, body)
	}
}

// TestRESTSandboxStatusRoute verifies GET /v1/sandboxes/{id} reaches sandbox_status.
func TestRESTSandboxStatusRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := getJSON(t, srv, "/v1/sandboxes/00000000-0000-0000-0000-000000000001")
	// store lookup returns error with nil pool
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d: %v", code, body)
	}
}

// TestRESTExecNoAuth verifies POST /v1/sandboxes/{id}/exec rejects unauthenticated requests
// when no sandbox_token is provided.
func TestRESTExecNoAuth(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := doReq(t, srv, "POST", "/v1/sandboxes/00000000-0000-0000-0000-000000000001/exec", map[string]any{
		"command": "echo hello",
	})
	if code != 401 {
		t.Fatalf("expected 401, got %d: %v", code, body)
	}
}

// TestRESTExecWithSandboxToken verifies POST /v1/sandboxes/{id}/exec accepts sandbox_token in body.
func TestRESTExecWithSandboxToken(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := doReq(t, srv, "POST", "/v1/sandboxes/00000000-0000-0000-0000-000000000001/exec", map[string]any{
		"command":       "echo hello",
		"sandbox_token": "sbx_abc123",
	})
	// Should NOT be 401 — token-based callerKey should pass the auth check.
	// It'll fail deeper (store error or sandbox not found), which is fine.
	if code == 401 {
		t.Fatalf("sandbox_token in body should bypass 401, got: %v", body)
	}
}

// TestRESTExecResultNoAuth verifies GET /v1/executions/{id} rejects without auth.
func TestRESTExecResultNoAuth(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, body := getJSON(t, srv, "/v1/executions/exec_abc123")
	if code != 401 {
		t.Fatalf("expected 401, got %d: %v", code, body)
	}
	_ = body
}

// TestRESTExecResultWithToken verifies GET /v1/executions/{id}?sandbox_token=sbx_... passes auth.
func TestRESTExecResultWithToken(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := getJSON(t, srv, "/v1/executions/exec_abc123?sandbox_token=sbx_test")
	// Should get past auth (not 401). Likely 404 (exec not found) since registry is empty.
	if code == 401 {
		t.Fatal("sandbox_token in query should bypass 401")
	}
	if code != 404 {
		t.Fatalf("expected 404 (exec not found), got %d", code)
	}
}

// TestRESTExecCancelNoAuth verifies DELETE /v1/executions/{id} rejects without auth.
func TestRESTExecCancelNoAuth(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := doReq(t, srv, "DELETE", "/v1/executions/exec_abc123", nil)
	if code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}
}

// TestRESTPauseRoute verifies POST /v1/sandboxes/{id}/pause reaches the handler.
func TestRESTPauseRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := doReq(t, srv, "POST", "/v1/sandboxes/00000000-0000-0000-0000-000000000001/pause", nil)
	// store error with nil pool
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d", code)
	}
}

// TestRESTResumeRoute verifies POST /v1/sandboxes/{id}/resume reaches the handler.
func TestRESTResumeRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := doReq(t, srv, "POST", "/v1/sandboxes/00000000-0000-0000-0000-000000000001/resume", nil)
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d", code)
	}
}

// TestRESTTerminateRoute verifies DELETE /v1/sandboxes/{id} reaches the handler.
func TestRESTTerminateRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := doReq(t, srv, "DELETE", "/v1/sandboxes/00000000-0000-0000-0000-000000000001", nil)
	// store error with nil pool
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d", code)
	}
}

// TestRESTWriteFileRoute verifies POST /v1/sandboxes/{id}/files/write reaches the handler.
func TestRESTWriteFileRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := doReq(t, srv, "POST", "/v1/sandboxes/00000000-0000-0000-0000-000000000001/files/write", map[string]any{
		"path":    "/test.txt",
		"content": "hello",
	})
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d", code)
	}
}

// TestRESTReadFileRoute verifies POST /v1/sandboxes/{id}/files/read reaches the handler.
func TestRESTReadFileRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := doReq(t, srv, "POST", "/v1/sandboxes/00000000-0000-0000-0000-000000000001/files/read", map[string]any{
		"path": "/test.txt",
	})
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d", code)
	}
}

// TestRESTListFilesRoute verifies GET /v1/sandboxes/{id}/files reaches the handler.
func TestRESTListFilesRoute(t *testing.T) {
	srv, close := testRouter(t)
	defer close()
	code, _ := getJSON(t, srv, "/v1/sandboxes/00000000-0000-0000-0000-000000000001/files")
	if code != 500 {
		t.Fatalf("expected 500 (store_error), got %d", code)
	}
}
