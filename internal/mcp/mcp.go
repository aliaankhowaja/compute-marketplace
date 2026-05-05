// Package mcp implements a stateless MCP Streamable HTTP (JSON-RPC) surface for the control plane.
package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	protocolVersion2024   = "2024-11-05"
	protocolVersion202506 = "2025-06-18"
	protocolVersion2025   = "2025-11-25"
)

// protocolVersionDefault is returned when the client omits protocolVersion on initialize.
const protocolVersionDefault = protocolVersion2025

var supportedProtocolVersions = []string{
	protocolVersion2024,
	protocolVersion202506,
	protocolVersion2025,
}

func negotiateProtocolVersion(clientRequested string) (string, error) {
	c := strings.TrimSpace(clientRequested)
	if c == "" {
		return protocolVersionDefault, nil
	}
	for _, v := range supportedProtocolVersions {
		if c == v {
			return v, nil
		}
	}
	return "", fmt.Errorf("unsupported protocol version %q (supported: %s)", c, strings.Join(supportedProtocolVersions, ", "))
}

// HealthPayload is the GET /v1/mcp/health JSON body.
func HealthPayload() map[string]any {
	return map[string]any{
		"status":            "ok",
		"protocol_version":  protocolVersionDefault,
		"transport":         "streamable-http",
		"json_rpc_per_post": true,
	}
}

// JSON-RPC 2.0 error codes (plans/07-mcp-server.md)
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
	ErrCodeAuth           = -32001
	ErrCodePayment        = -32002
	ErrCodeNotFound       = -32003
	ErrCodeConflict       = -32004
	ErrCodeRateLimited    = -32005
	ErrCodeUnavailable    = -32006
)

// Request is a single JSON-RPC request object.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *ErrorObj       `json:"error,omitempty"`
}

// ErrorObj is JSON-RPC error.
type ErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// rpcErr builds a JSON-RPC error from HTTP-style semantics.
func rpcErr(id json.RawMessage, httpStatus int, slug, msg string, extra map[string]any) Response {
	code, m := mapHTTPToRPC(httpStatus, msg)
	data := map[string]any{"error_slug": slug}
	for k, v := range extra {
		data[k] = v
	}
	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &ErrorObj{Code: code, Message: m, Data: data},
	}
}

func mapHTTPToRPC(httpStatus int, msg string) (int, string) {
	switch httpStatus {
	case http.StatusBadRequest:
		return ErrCodeInvalidParams, msg
	case http.StatusUnauthorized:
		return ErrCodeAuth, msg
	case http.StatusPaymentRequired:
		return ErrCodePayment, msg
	case http.StatusForbidden:
		return ErrCodeAuth, msg
	case http.StatusNotFound:
		return ErrCodeNotFound, msg
	case http.StatusConflict:
		return ErrCodeConflict, msg
	case http.StatusTooManyRequests:
		return ErrCodeRateLimited, msg
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return ErrCodeUnavailable, msg
	default:
		if httpStatus >= 500 {
			return ErrCodeInternal, msg
		}
		return ErrCodeInvalidParams, msg
	}
}

func rpcOK(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

// toolErrorResult returns an MCP tools/call payload with isError: true so clients surface
// invoice/auth details in visible tool content (not only JSON-RPC error.data).
func toolErrorResult(slug, msg string, extra map[string]any) map[string]any {
	text := buildToolErrorText(slug, msg, extra)
	data := map[string]any{"error_slug": slug}
	for k, v := range extra {
		data[k] = v
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"isError":           true,
		"structuredContent": data,
	}
}

func buildToolErrorText(slug, msg string, extra map[string]any) string {
	msg = strings.TrimSpace(msg)
	if slug == "payment_required" && extra != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "payment_required")
		if msg != "" {
			fmt.Fprintf(&b, "\n\n%s\n", msg)
		}
		appendIf := func(label string, key string) {
			s := stringifyExtraVal(extra[key])
			if s == "" {
				return
			}
			fmt.Fprintf(&b, "\n%s:\n%s\n", label, s)
		}
		appendIf("BOLT11 invoice (copy this full string)", "invoice")
		appendIf("Macaroon", "macaroon")
		appendIf("payment_hash (hex)", "payment_hash")
		appendIf("agent_id", "agent_id")
		appendIf("expires_at", "expires_at")
		if nf := stringifyExtraVal(extra["retry_authorization_format"]); nf != "" {
			fmt.Fprintf(&b, "\nRetry Authorization format:\n%s\n", nf)
		}
		if ns := stringifyExtraVal(extra["next_step"]); ns != "" {
			fmt.Fprintf(&b, "\nNext step:\n%s\n", ns)
		}
		fmt.Fprintf(&b, "\nAfter paying the invoice, retry sandbox_create and include on every MCP POST:\n"+
			`Authorization: L402 <macaroon>:<preimage_hex>`+"\n"+
			"Use macaroon from above and preimage from your Lightning payment (32-byte preimage as lowercase hex).\n")
		return strings.TrimRight(strings.TrimSpace(b.String()), "\n")
	}
	switch {
	case slug != "" && msg != "":
		return fmt.Sprintf("%s: %s", slug, msg)
	case msg != "":
		return msg
	default:
		return slug
	}
}

func stringifyExtraVal(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		return fmt.Sprintf("%v", t)
	case float32:
		return fmt.Sprintf("%v", t)
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case uint64:
		return fmt.Sprintf("%d", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		s := strings.TrimSpace(string(b))
		if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
			var decoded string
			if err := json.Unmarshal(b, &decoded); err == nil {
				return strings.TrimSpace(decoded)
			}
		}
		return s
	}
}

func slugFromPaymentClass(class string) string {
	if class == "" {
		return "payment_error"
	}
	return class
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rewrapToolResult wraps a plain object as MCP content (structured).
func rewrapToolResult(v any) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": mustJSON(v)},
		},
		"structuredContent": v,
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
