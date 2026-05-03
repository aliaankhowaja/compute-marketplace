package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

// Handler serves one JSON-RPC 2.0 message per POST (Streamable HTTP MVP).
func Handler(svc *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		limit := int64(4 << 20)
		if svc != nil && svc.Cfg != nil && svc.Cfg.MCPRequestBodyLimit > 0 {
			limit = svc.Cfg.MCPRequestBodyLimit
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)

		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusOK, Response{
				JSONRPC: "2.0",
				Error:   &ErrorObj{Code: ErrCodeParseError, Message: err.Error()},
			})
			return
		}
		if req.JSONRPC != "2.0" {
			writeJSON(w, http.StatusOK, Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &ErrorObj{Code: ErrCodeInvalidRequest, Message: "jsonrpc must be 2.0"},
			})
			return
		}
		// JSON-RPC notification (no id): empty body response
		if len(req.ID) == 0 || string(req.ID) == "null" {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		p := auth.PrincipalFrom(r.Context())

		switch strings.TrimSpace(req.Method) {
		case "initialize":
			sid := uuid.NewString()
			w.Header().Set("Mcp-Session-Id", sid)
			writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "sockt-cp",
					"version": "mvp",
				},
				"session": map[string]any{
					"id": sid,
				},
			}))

		case "tools/list":
			writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
				"tools": toolList(),
			}))

		case "tools/call":
			var tcp struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &tcp); err != nil {
				writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusBadRequest, "invalid_params", err.Error(), nil))
				return
			}
			if strings.TrimSpace(tcp.Name) == "" {
				writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusBadRequest, "invalid_params", "tool name required", nil))
				return
			}
			args := tcp.Arguments
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage(`{}`)
			}
			res, st, slug, extra := svc.CallTool(r.Context(), p, tcp.Name, args)
			if st == 0 && slug == "" && res == nil {
				writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusBadRequest, "unknown_tool", "unknown tool", map[string]any{"tool": tcp.Name}))
				return
			}
			if st != 0 {
				msg := slug
				if extra != nil && extra["message"] != nil {
					if m, ok := extra["message"].(string); ok && m != "" {
						msg = m
					}
				}
				writeJSON(w, http.StatusOK, rpcErr(req.ID, st, slug, msg, extra))
				return
			}
			writeJSON(w, http.StatusOK, rpcOK(req.ID, rewrapToolResult(res)))

		case "ping":
			writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{}))

		default:
			writeJSON(w, http.StatusOK, Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &ErrorObj{Code: ErrCodeMethodNotFound, Message: "method not found", Data: map[string]any{"method": req.Method}},
			})
		}
	})
}
