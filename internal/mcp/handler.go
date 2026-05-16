package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

func rpcErrMessageFromExtra(slug string, extra map[string]any) string {
	if extra == nil {
		return slug
	}
	if m, ok := extra["message"].(string); ok && strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	if b, ok := extra["body"].(string); ok && strings.TrimSpace(b) != "" {
		return strings.TrimSpace(b)
	}
	if extra["status"] != nil {
		return fmt.Sprintf("%s (HTTP %v)", slug, extra["status"])
	}
	return slug
}

// SSEHandler serves legacy MCP SSE: GET opens a stream advertising the JSON-RPC POST endpoint URL.
func SSEHandler(postEndpointURL string) http.Handler {
	postEndpointURL = strings.TrimSpace(postEndpointURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if postEndpointURL == "" {
			http.Error(w, "server misconfigured", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", postEndpointURL)
		fl.Flush()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprintf(w, ": ping\n\n")
				fl.Flush()
			}
		}
	})
}

// Handler serves one JSON-RPC 2.0 message per POST (Streamable HTTP MVP).
func Handler(svc *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var initParams struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if len(req.Params) > 0 && string(req.Params) != "null" {
				if err := json.Unmarshal(req.Params, &initParams); err != nil {
					writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusBadRequest, "invalid_params", err.Error(), nil))
					return
				}
			}
			pv, err := negotiateProtocolVersion(initParams.ProtocolVersion)
			if err != nil {
				writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusBadRequest, "unsupported_protocol_version", err.Error(), nil))
				return
			}
			sid := uuid.NewString()
			w.Header().Set("Mcp-Session-Id", sid)

			if svc != nil && svc.Sessions != nil {
				svc.Sessions.Create(sid, p)
			}

			caps := map[string]any{
				"tools": map[string]any{},
			}
			cpURL := ""
			if svc != nil && svc.Cfg != nil {
				cpURL = strings.TrimRight(strings.TrimSpace(svc.Cfg.ControlPlanePublicURL), "/")
			}
			if cpURL != "" {
				caps["notifications"] = map[string]any{
					"endpoint":  cpURL + "/v1/mcp",
					"transport": "sse",
					"header":    "Mcp-Session-Id: " + sid,
				}
			}
			writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
				"protocolVersion": pv,
				"capabilities":    caps,
				"serverInfo": map[string]any{
					"name":    "sockt-cp",
					"version": "mvp",
				},
				"session": map[string]any{
					"id": sid,
				},
				"instructions": sandboxInstructions(svc),
				"sockt":        sessionHints(svc, p),
			}))

		case "tools/list":
			writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
				"tools": toolList(svc, p),
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

			sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
			if sessionID == "" {
				writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusBadRequest, "session_required",
					"Mcp-Session-Id header required for tools/call", nil))
				return
			}
			var session *Session
			if svc != nil && svc.Sessions != nil {
				session = svc.Sessions.Get(sessionID)
			}
			if session == nil {
				writeJSON(w, http.StatusOK, rpcErr(req.ID, http.StatusUnauthorized, "invalid_session",
					"session not found or expired", nil))
				return
			}

			svc.ServeToolCallSSE(w, r, session, p, tcp.Name, args, req.ID)

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
