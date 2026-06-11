package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/mcp"
)

func writeRESTResponse(w http.ResponseWriter, result any, httpStatus int, slug string, extra map[string]any) {
	if httpStatus != 0 {
		msg := slug
		if extra != nil {
			if m, ok := extra["message"].(string); ok && m != "" {
				msg = m
			}
		}
		writeErr(w, httpStatus, slug, msg)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func sandboxTokenFromRequest(r *http.Request) string {
	if tok := r.URL.Query().Get("sandbox_token"); tok != "" {
		return tok
	}
	return ""
}

func handleRESTListTiers(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		args, _ := json.Marshal(map[string]any{})
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_list_tiers", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTCreateSandbox(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		args, _ := json.Marshal(body)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_create", args)
		if httpStatus != 0 {
			writeRESTResponse(w, result, httpStatus, slug, extra)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}
}

func handleRESTExec(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		callerKey := mcp.CallerKeyFromPrincipal(p)
		if callerKey == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "authentication required for exec operations")
			return
		}
		sandboxID := r.PathValue("id")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		body["sandbox_id"] = sandboxID
		if tok := sandboxTokenFromRequest(r); tok != "" {
			body["sandbox_token"] = tok
		}
		args, _ := json.Marshal(body)
		result, httpStatus, slug, extra := svc.CallToolAsyncREST(r.Context(), callerKey, p, "sandbox_exec", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTExecResult(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		callerKey := mcp.CallerKeyFromPrincipal(p)
		if callerKey == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		executionID := r.PathValue("execution_id")
		m := map[string]any{"execution_id": executionID}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallToolAsyncREST(r.Context(), callerKey, p, "sandbox_exec_result", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTExecCancel(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		callerKey := mcp.CallerKeyFromPrincipal(p)
		if callerKey == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		executionID := r.PathValue("execution_id")
		m := map[string]any{"execution_id": executionID}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallToolAsyncREST(r.Context(), callerKey, p, "sandbox_exec_cancel", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTWriteFile(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		body["sandbox_id"] = sandboxID
		if tok := sandboxTokenFromRequest(r); tok != "" {
			body["sandbox_token"] = tok
		}
		args, _ := json.Marshal(body)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_write_file", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTReadFile(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		body["sandbox_id"] = sandboxID
		if tok := sandboxTokenFromRequest(r); tok != "" {
			body["sandbox_token"] = tok
		}
		args, _ := json.Marshal(body)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_read_file", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTListFiles(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		m := map[string]any{"sandbox_id": sandboxID}
		if path := r.URL.Query().Get("path"); path != "" {
			m["path"] = path
		}
		if r.URL.Query().Get("recursive") == "true" {
			m["recursive"] = true
		}
		if v := r.URL.Query().Get("max_depth"); v != "" {
			if d, err := strconv.Atoi(v); err == nil {
				m["max_depth"] = d
			}
		}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_list_files", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTSandboxStatus(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		m := map[string]any{"sandbox_id": sandboxID}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_status", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTPause(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		m := map[string]any{"sandbox_id": sandboxID}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_pause", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTResume(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		m := map[string]any{"sandbox_id": sandboxID}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_resume", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}

func handleRESTTerminate(svc *mcp.Service) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		sandboxID := r.PathValue("id")
		m := map[string]any{"sandbox_id": sandboxID, "confirm": true}
		if r.Body != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				if addr, ok := body["lightning_address"].(string); ok && strings.TrimSpace(addr) != "" {
					m["lightning_address"] = addr
				}
				if tok, ok := body["sandbox_token"].(string); ok && tok != "" {
					m["sandbox_token"] = tok
				}
			}
		}
		if tok := sandboxTokenFromRequest(r); tok != "" {
			m["sandbox_token"] = tok
		}
		args, _ := json.Marshal(m)
		result, httpStatus, slug, extra := svc.CallTool(r.Context(), p, "sandbox_terminate", args)
		writeRESTResponse(w, result, httpStatus, slug, extra)
	}
}
