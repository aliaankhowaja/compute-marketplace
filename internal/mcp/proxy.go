package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/orchestrator"
	"github.com/aliaankhowaja/sockt-backend/internal/runtime"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

func dataPlaneBase(sb *store.Sandbox) string {
	if sb == nil {
		return ""
	}
	if sb.PodProxyURL != nil && strings.TrimSpace(*sb.PodProxyURL) != "" {
		return strings.TrimRight(strings.TrimSpace(*sb.PodProxyURL), "/")
	}
	if sb.PodID != nil && strings.TrimSpace(*sb.PodID) != "" {
		return strings.TrimRight(orchestrator.RunPodProxyURL(nil, *sb.PodID), "/")
	}
	return ""
}

func (s *Service) proxyDataJWT(sb *store.Sandbox, scope string) (string, error) {
	if sb.PodID == nil || strings.TrimSpace(*sb.PodID) == "" {
		return "", fmt.Errorf("pod not ready")
	}
	ttl := s.Cfg.DataJWTTTL
	if s.Cfg.MCPDataJWTTTL > 0 {
		ttl = s.Cfg.MCPDataJWTTTL
	}
	return auth.MintDataJWT(s.Cfg.WSSJWTSecret, sb.ID.String(), strings.TrimSpace(*sb.PodID), scope, ttl, s.Cfg.DataJWTAudience)
}

type execRuntimeRequest struct {
	Argv           []string `json:"argv"`
	CWD            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type execRuntimeResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type writeFileRuntimeRequest struct {
	Path          string `json:"path"`
	Mode          uint32 `json:"mode"`
	ContentBase64 string `json:"content_base64"`
	CreateDirs    bool   `json:"create_dirs"`
}

type readFileRuntimeResponse struct {
	Path    string `json:"path"`
	Content string `json:"content_base64"`
}

type listFilesRuntimeResponse struct {
	Dir   string      `json:"dir"`
	Files []fileEntry `json:"files"`
}

type fileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModUnix int64  `json:"mod_unix"`
	IsDir   bool   `json:"is_dir"`
}

type podStatusJSON struct {
	Service string          `json:"service"`
	Version string          `json:"version,omitempty"`
	Balance json.RawMessage `json:"balance"`
}

func (s *Service) proxyPOST(ctx context.Context, base, path, token string, body any) ([]byte, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return raw, resp.StatusCode, err
}

func (s *Service) proxyGET(ctx context.Context, baseURL, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return raw, resp.StatusCode, err
}

func (s *Service) httpClient() *http.Client {
	if s != nil && s.HTTPClient != nil {
		return s.HTTPClient
	}
	to := 120 * time.Second
	if s != nil && s.Cfg != nil && s.Cfg.MCPProxyTimeout > 0 {
		to = s.Cfg.MCPProxyTimeout
	}
	return &http.Client{Timeout: to}
}

func (s *Service) toolExec(ctx context.Context, sb *store.Sandbox, args map[string]any) (any, int, string, map[string]any) {
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, http.StatusBadRequest, "pod_unreachable", map[string]any{"message": "no data plane endpoint"}
	}
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "command required"}
	}
	timeoutMS := 30000.0
	if v, ok := args["timeout_ms"].(float64); ok && v > 0 {
		timeoutMS = v
	}
	sec := int(timeoutMS / 1000)
	if sec <= 0 {
		sec = 1
	}
	if sec > 300 {
		sec = 300
	}
	cwd := "."
	if v, ok := args["working_dir"].(string); ok && strings.TrimSpace(v) != "" {
		cwd = v
	}
	tok, err := s.proxyDataJWT(sb, runtime.ScopeExec)
	if err != nil {
		return nil, http.StatusInternalServerError, "token_error", map[string]any{"message": err.Error()}
	}
	path := fmt.Sprintf("/sandbox/%s/exec", sb.ID.String())
	body := execRuntimeRequest{
		Argv:           []string{"/bin/bash", "-lc", cmd},
		CWD:            cwd,
		TimeoutSeconds: sec,
	}
	raw, code, err := s.proxyPOST(ctx, base, path, tok, body)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "host_unreachable", map[string]any{"message": err.Error()}
	}
	if code < 200 || code >= 300 {
		st, ex := hostHTTPFailure(code, raw)
		return nil, st, "host_error", ex
	}
	var out execRuntimeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, http.StatusBadGateway, "host_error", map[string]any{"message": err.Error()}
	}
	return map[string]any{
		"stdout":      out.Stdout,
		"stderr":      out.Stderr,
		"exit_code":   out.ExitCode,
		"duration_ms": 0,
		"timed_out":   false,
	}, 0, "", nil
}

func decodeJSONOrText(raw []byte, httpCode int) (map[string]any, string) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		return m, ""
	}
	return nil, strings.TrimSpace(string(raw))
}

// statusFromDataPlane maps runtime HTTP codes to MCP-facing statuses so mapHTTPToRPC
// yields sensible JSON-RPC error codes (e.g. 404 → not found, not unavailable).
func statusFromDataPlane(code int) int {
	switch code {
	case http.StatusBadRequest:
		return http.StatusBadRequest
	case http.StatusUnauthorized:
		return http.StatusUnauthorized
	case http.StatusForbidden:
		return http.StatusForbidden
	case http.StatusNotFound:
		return http.StatusNotFound
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func hostHTTPFailure(code int, raw []byte) (httpStatus int, extra map[string]any) {
	_, textErr := decodeJSONOrText(raw, code)
	msg := strings.TrimSpace(textErr)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", code)
	}
	ex := map[string]any{"message": msg, "status": code}
	st := statusFromDataPlane(code)
	if code == http.StatusNotFound {
		ex["hint"] = "Path sandbox id must match the runtime SANDBOX_ID. Check PodProxyURL/pod_id for this sandbox row, or reprovision if the pod was swapped."
	}
	return st, ex
}

func (s *Service) toolWriteFile(ctx context.Context, sb *store.Sandbox, args map[string]any) (any, int, string, map[string]any) {
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, http.StatusBadRequest, "pod_unreachable", map[string]any{"message": "no data plane endpoint"}
	}
	pathStr, _ := args["path"].(string)
	if strings.TrimSpace(pathStr) == "" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "path required"}
	}
	content, _ := args["content"].(string)
	enc := "utf8"
	if v, ok := args["encoding"].(string); ok && v != "" {
		enc = strings.ToLower(strings.TrimSpace(v))
	}
	var b64 string
	switch enc {
	case "base64":
		b64 = strings.TrimSpace(content)
	default:
		b64 = base64.StdEncoding.EncodeToString([]byte(content))
	}
	createDirs := false
	if v, ok := args["create_dirs"].(bool); ok {
		createDirs = v
	}
	tok, err := s.proxyDataJWT(sb, runtime.ScopeFiles)
	if err != nil {
		return nil, http.StatusInternalServerError, "token_error", map[string]any{"message": err.Error()}
	}
	body := writeFileRuntimeRequest{
		Path:          pathStr,
		Mode:          0o644,
		ContentBase64: b64,
		CreateDirs:    createDirs,
	}
	apiPath := fmt.Sprintf("/sandbox/%s/files", sb.ID.String())
	raw, code, err := s.proxyPOST(ctx, base, apiPath, tok, body)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "host_unreachable", map[string]any{"message": err.Error()}
	}
	if code < 200 || code >= 300 {
		st, ex := hostHTTPFailure(code, raw)
		return nil, st, "host_error", ex
	}
	bw := int64(len(b64)) // approx bytes on wire
	return map[string]any{
		"path":          pathStr,
		"bytes_written": bw,
		"created_dirs":  createDirs,
	}, 0, "", nil
}

func (s *Service) toolReadFile(ctx context.Context, sb *store.Sandbox, args map[string]any) (any, int, string, map[string]any) {
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, http.StatusBadRequest, "pod_unreachable", map[string]any{"message": "no data plane endpoint"}
	}
	pathStr, _ := args["path"].(string)
	if strings.TrimSpace(pathStr) == "" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "path required"}
	}
	enc := "utf8"
	if v, ok := args["encoding"].(string); ok && v != "" {
		enc = strings.ToLower(strings.TrimSpace(v))
	}
	tok, err := s.proxyDataJWT(sb, runtime.ScopeFiles)
	if err != nil {
		return nil, http.StatusInternalServerError, "token_error", map[string]any{"message": err.Error()}
	}
	u, err := url.Parse(base + fmt.Sprintf("/sandbox/%s/files", sb.ID.String()))
	if err != nil {
		return nil, http.StatusInternalServerError, "internal", nil
	}
	q := u.Query()
	q.Set("path", pathStr)
	u.RawQuery = q.Encode()
	raw, code, err := s.proxyGET(ctx, u.String(), tok)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "host_unreachable", map[string]any{"message": err.Error()}
	}
	if code < 200 || code >= 300 {
		st, ex := hostHTTPFailure(code, raw)
		return nil, st, "host_error", ex
	}
	var rf readFileRuntimeResponse
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, http.StatusBadGateway, "host_error", map[string]any{"message": err.Error()}
	}
	decoded, err := base64.StdEncoding.DecodeString(rf.Content)
	if err != nil {
		return nil, http.StatusBadGateway, "host_error", map[string]any{"message": "invalid base64 from runtime"}
	}
	out := map[string]any{
		"path":        rf.Path,
		"size_bytes":  len(decoded),
		"truncated":   false,
		"encoding":    enc,
		"content":     "",
		"content_b64": "",
	}
	maxB := 0
	if v, ok := args["max_bytes"].(float64); ok {
		maxB = int(v)
	}
	final := decoded
	if maxB > 0 && len(final) > maxB {
		final = final[:maxB]
		out["truncated"] = true
	}
	switch enc {
	case "base64":
		out["content_b64"] = base64.StdEncoding.EncodeToString(final)
	default:
		out["content"] = string(final)
	}
	return out, 0, "", nil
}

func (s *Service) toolListFiles(ctx context.Context, sb *store.Sandbox, args map[string]any) (any, int, string, map[string]any) {
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, http.StatusBadRequest, "pod_unreachable", map[string]any{"message": "no data plane endpoint"}
	}
	dir := "/"
	if v, ok := args["path"].(string); ok && strings.TrimSpace(v) != "" {
		dir = v
	}
	tok, err := s.proxyDataJWT(sb, runtime.ScopeFiles)
	if err != nil {
		return nil, http.StatusInternalServerError, "token_error", map[string]any{"message": err.Error()}
	}
	u, err := url.Parse(base + fmt.Sprintf("/sandbox/%s/files/list", sb.ID.String()))
	if err != nil {
		return nil, http.StatusInternalServerError, "internal", nil
	}
	q := u.Query()
	q.Set("dir", dir)
	u.RawQuery = q.Encode()
	raw, code, err := s.proxyGET(ctx, u.String(), tok)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "host_unreachable", map[string]any{"message": err.Error()}
	}
	if code < 200 || code >= 300 {
		st, ex := hostHTTPFailure(code, raw)
		return nil, st, "host_error", ex
	}
	var lr listFilesRuntimeResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return nil, http.StatusBadGateway, "host_error", map[string]any{"message": err.Error()}
	}
	entries := make([]map[string]any, 0, len(lr.Files))
	for _, e := range lr.Files {
		entries = append(entries, map[string]any{
			"name":    e.Name,
			"size":    e.Size,
			"mod_ts":  e.ModUnix,
			"is_dir":  e.IsDir,
			"is_file": !e.IsDir,
		})
	}
	return map[string]any{"entries": entries, "total_entries": len(entries)}, 0, "", nil
}

func (s *Service) toolRuntimeStatus(ctx context.Context, sb *store.Sandbox) (map[string]any, int, string, map[string]any) {
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, 0, "", nil
	}
	tok, err := s.proxyDataJWT(sb, runtime.ScopeExec)
	if err != nil {
		return nil, 0, "", nil
	}
	u := base + fmt.Sprintf("/sandbox/%s/status", sb.ID.String())
	raw, code, err := s.proxyGET(ctx, u, tok)
	if err != nil || code < 200 || code >= 300 {
		return nil, 0, "", nil
	}
	var rs podStatusJSON
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, 0, "", nil
	}
	out := map[string]any{}
	_ = json.Unmarshal(rs.Balance, &out)
	return out, 0, "", nil
}

func parseUUIDArg(args map[string]any, key string) (uuid.UUID, bool) {
	raw, ok := args[key].(string)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func argFloat(args map[string]any, key string) float64 {
	if v, ok := args[key].(float64); ok {
		return v
	}
	if v, ok := args[key].(json.Number); ok {
		f, _ := v.Float64()
		return f
	}
	if i, ok := args[key].(int); ok {
		return float64(i)
	}
	if s, ok := args[key].(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err == nil {
			return f
		}
	}
	return 0
}
