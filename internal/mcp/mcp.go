// Package mcp implements a stateless MCP Streamable HTTP (JSON-RPC) surface for the control plane.
package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const protocolVersion = "2025-11"

// HealthPayload is the GET /v1/mcp/health JSON body.
func HealthPayload() map[string]any {
	return map[string]any{
		"status":            "ok",
		"protocol_version":  protocolVersion,
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
