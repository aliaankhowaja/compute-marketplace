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
	Service            string          `json:"service"`
	Version            string          `json:"version,omitempty"`
	Balance            json.RawMessage `json:"balance"`
	NegativeGraceSecs  int64           `json:"negative_grace_secs,omitempty"`
	GraceRemainingSecs int64           `json:"grace_remaining_secs,omitempty"`
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

func (s *Service) proxyPOSTWithTimeout(ctx context.Context, base, path, token string, body any, timeout time.Duration) ([]byte, int, error) {
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
	resp, err := s.httpClientForTimeout(timeout).Do(req)
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

// httpClientForTimeout returns a client whose timeout accommodates a known long-running request.
// A zero duration means no timeout (used for async streaming exec).
func (s *Service) httpClientForTimeout(d time.Duration) *http.Client {
	if s != nil && s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: d}
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
	// Use exec-specific timeout: command timeout + 10s buffer for network overhead.
	execTimeout := time.Duration(sec+10) * time.Second
	execCtx, execCancel := context.WithTimeout(ctx, execTimeout)
	defer execCancel()
	raw, code, err := s.proxyPOSTWithTimeout(execCtx, base, path, tok, body, execTimeout)
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

func (s *Service) toolExecAsync(session *Session, sb *store.Sandbox, args map[string]any) (any, int, string, map[string]any) {
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, http.StatusBadRequest, "pod_unreachable", map[string]any{"message": "no data plane endpoint"}
	}
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "command required"}
	}
	cwd := "."
	if v, ok := args["working_dir"].(string); ok && strings.TrimSpace(v) != "" {
		cwd = v
	}

	maxTimeout := 4 * time.Hour
	if s.Cfg != nil && s.Cfg.MCPExecMaxTimeout > 0 {
		maxTimeout = s.Cfg.MCPExecMaxTimeout
	}

	maxConc := 10
	if s.Cfg != nil && s.Cfg.MCPMaxConcurrentExec > 0 {
		maxConc = s.Cfg.MCPMaxConcurrentExec
	}
	if s.Executions.CountForSession(session.ID) >= maxConc {
		return nil, http.StatusTooManyRequests, "too_many_executions", map[string]any{
			"message": fmt.Sprintf("max %d concurrent executions per session", maxConc),
		}
	}

	execCtx, execCancel := context.WithTimeout(context.Background(), maxTimeout)
	exec := s.Executions.Create(session.ID, sb.ID, cmd, execCancel)
	s.Sessions.TrackSandbox(session.ID, sb.ID)

	go s.runExecAsync(execCtx, execCancel, exec, session, sb, cmd, cwd)

	return map[string]any{
		"execution_id": exec.ID,
		"status":       "running",
		"sandbox_id":   sb.ID.String(),
		"command":      cmd,
	}, 0, "", nil
}

func (s *Service) runExecAsync(ctx context.Context, cancel context.CancelFunc, exec *Execution, session *Session, sb *store.Sandbox, cmd, cwd string) {
	defer cancel()

	base := dataPlaneBase(sb)
	tok, err := s.proxyDataJWT(sb, runtime.ScopeExec)
	if err != nil {
		s.Executions.Complete(exec.ID, "failed")
		exec.SetError(err.Error())
		session.Emit(SessionEvent{
			Type:      "exec.error",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"error": err.Error(), "slug": "token_error"},
		})
		return
	}

	streamPath := fmt.Sprintf("/sandbox/%s/exec/stream", sb.ID.String())
	body := execRuntimeRequest{
		Argv:           []string{"/bin/bash", "-lc", cmd},
		CWD:            cwd,
		TimeoutSeconds: 0,
	}

	streamed := s.tryStreamingExec(ctx, exec, session, sb, base, streamPath, tok, body)
	if streamed {
		return
	}

	// Fallback: use standard /exec with long timeout
	fallbackPath := fmt.Sprintf("/sandbox/%s/exec", sb.ID.String())
	fallbackBody := execRuntimeRequest{
		Argv:           []string{"/bin/bash", "-lc", cmd},
		CWD:            cwd,
		TimeoutSeconds: 0, // unlimited on runtime side; ctx controls cancellation
	}
	raw, code, err := s.proxyPOSTWithTimeout(ctx, base, fallbackPath, tok, fallbackBody, 0)
	if err != nil {
		status := "failed"
		if ctx.Err() != nil {
			status = "cancelled"
		}
		s.Executions.Complete(exec.ID, status)
		exec.SetError(err.Error())
		session.Emit(SessionEvent{
			Type:      "exec.error",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"error": err.Error(), "slug": "host_unreachable"},
		})
		return
	}
	if code < 200 || code >= 300 {
		s.Executions.Complete(exec.ID, "failed")
		_, textErr := decodeJSONOrText(raw, code)
		exec.SetError(textErr)
		session.Emit(SessionEvent{
			Type:      "exec.error",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"error": textErr, "slug": "host_error", "http_status": code},
		})
		return
	}

	var out execRuntimeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		s.Executions.Complete(exec.ID, "failed")
		exec.SetError("invalid response from runtime")
		session.Emit(SessionEvent{
			Type:      "exec.error",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"error": "invalid response from runtime"},
		})
		return
	}

	s.Executions.Complete(exec.ID, "completed")
	exec.SetDone(out.ExitCode)
	if out.Stdout != "" {
		exec.AppendOutput("stdout", out.Stdout)
		session.Emit(SessionEvent{
			Type:      "exec.output",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"stream": "stdout", "chunk": out.Stdout},
		})
	}
	if out.Stderr != "" {
		exec.AppendOutput("stderr", out.Stderr)
		session.Emit(SessionEvent{
			Type:      "exec.output",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"stream": "stderr", "chunk": out.Stderr},
		})
	}
	session.Emit(SessionEvent{
		Type:      "exec.done",
		ExecID:    exec.ID,
		SandboxID: sb.ID,
		Data: map[string]any{
			"exit_code":   out.ExitCode,
			"duration_ms": time.Since(exec.StartedAt).Milliseconds(),
		},
	})
}

type streamChunk struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

func (s *Service) tryStreamingExec(ctx context.Context, exec *Execution, session *Session, sb *store.Sandbox, base, path, tok string, body execRuntimeRequest) bool {
	b, err := json.Marshal(body)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		s.Executions.Complete(exec.ID, "failed")
		_, textErr := decodeJSONOrText(raw, resp.StatusCode)
		session.Emit(SessionEvent{
			Type:      "exec.error",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"error": textErr, "slug": "host_error", "http_status": resp.StatusCode},
		})
		return true
	}

	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var chunk streamChunk
		if err := dec.Decode(&chunk); err != nil {
			break
		}
		switch chunk.Type {
		case "stdout", "stderr":
			exec.AppendOutput(chunk.Type, chunk.Data)
			session.Emit(SessionEvent{
				Type:      "exec.output",
				ExecID:    exec.ID,
				SandboxID: sb.ID,
				Data:      map[string]any{"stream": chunk.Type, "chunk": chunk.Data},
			})
		case "exit":
			exitCode := 0
			if chunk.ExitCode != nil {
				exitCode = *chunk.ExitCode
			}
			s.Executions.Complete(exec.ID, "completed")
			exec.SetDone(exitCode)
			session.Emit(SessionEvent{
				Type:      "exec.done",
				ExecID:    exec.ID,
				SandboxID: sb.ID,
				Data: map[string]any{
					"exit_code":   exitCode,
					"duration_ms": time.Since(exec.StartedAt).Milliseconds(),
				},
			})
			return true
		}
	}

	if exec.Status == "running" {
		s.Executions.Complete(exec.ID, "failed")
		exec.SetError("stream ended without exit")
		session.Emit(SessionEvent{
			Type:      "exec.error",
			ExecID:    exec.ID,
			SandboxID: sb.ID,
			Data:      map[string]any{"error": "stream ended without exit", "slug": "stream_incomplete"},
		})
	}
	return true
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
	if rs.GraceRemainingSecs > 0 {
		out["grace_remaining_secs"] = rs.GraceRemainingSecs
		out["negative_grace_secs"] = rs.NegativeGraceSecs
	}
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
