# Component 7: MCP Server

## Overview

The MCP (Model Context Protocol) Server is the `internal/mcp` package within the unified Go binary. It exposes all Satoshi Compute sandbox operations as MCP-compliant tools over an SSE (Server-Sent Events) transport, allowing any MCP-compatible LLM — Claude, Gemini, GPT-4 — to provision, operate, and pay for compute sandboxes natively as part of their tool-calling loop.

The MCP server runs inside the same process as the REST API. There is no separate MCP process, no IPC overhead — MCP tool calls invoke the same Go handler functions as the REST API.

---

## MCP Protocol Version

Implements the **MCP 2025-06-18 specification**.

- Transport: HTTP SSE (Server-Sent Events) — one long-lived GET connection for receiving messages, POST for sending
- Tool definitions: fully typed JSON Schema for all inputs and outputs
- Resource exposure: sandbox status and billing history exposed as MCP Resources
- No sampling or completion features used (tools only)

---

## Endpoints

The MCP server is mounted inside the unified Go binary alongside the REST API.

| Method | Path | Description |
|---|---|---|
| `GET` | `/mcp/sse` | Open SSE connection. Client receives `endpoint` event pointing to POST URL. |
| `POST` | `/mcp/message` | Send MCP messages (tool calls, list requests, initialize). |
| `GET` | `/mcp/health` | MCP server health check. Returns `{"status": "ok", "protocol_version": "2025-06-18"}` |

### Authentication

MCP connections require a valid agent JWT in the `Authorization: Bearer {jwt}` header on both the SSE GET and message POST requests. No L402 is required for MCP connections — the agent must have already completed L402 registration and received a JWT.

The underlying tool calls (e.g., `sandbox_create`) will trigger BOLT12 payments automatically via the billing engine — the LLM does not need to handle payment logic directly.

---

## MCP Server Initialization Sequence

1. Client sends `GET /mcp/sse` with agent JWT
2. Server sends SSE `endpoint` event: `{"endpoint": "/mcp/message?session_id={uuid}"}`
3. Client sends `POST /mcp/message` with body: `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude","version":"3.7"}}}`
4. Server responds: `{"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"resources":{}},"serverInfo":{"name":"satoshi-compute","version":"2.0.0"}}}`
5. Client sends `tools/list` — server returns all tool definitions
6. Client calls tools via `tools/call` messages

---

## Tool Definitions

### Tool: sandbox_create

**Description:** Provision a new compute sandbox environment. Automatically handles BOLT12 micropayment for the session. Returns credentials to interact with the sandbox.

**Input schema:**
- `tier` (string, required) — Enum: `nano`, `micro`, `standard`, `gpu_small`, `gpu_large`
- `template` (string, optional) — Enum: `ubuntu-base`, `python3`, `nodejs`, `rust`, `ml-cuda`
- `duration_secs` (integer, optional) — Estimated session length in seconds for budget pre-authorization
- `egress` (boolean, optional) — Enable outbound internet access (requires additional payment)
- `label` (string, optional) — Human label for this sandbox

**Output schema:**
- `sandbox_id` (string) — UUID of the provisioned sandbox
- `status` (string) — `pending` or `running`
- `tier` (string) — Confirmed tier
- `runtime` (string) — `firecracker` or `dedicated_vm`
- `template` (string) — Template used
- `wss_url` (string) — WebSocket URL for interactive shell
- `wss_token` (string) — Bearer token for WebSocket connection
- `sats_per_second` (number) — Billing rate
- `created_at` (string) — ISO8601 timestamp
- `estimated_ready_at` (string) — ISO8601 (may be future for dedicated VMs)

**Errors returned in MCP error format:**
- `INVALID_PARAMS` — unknown tier or template
- `RESOURCE_EXHAUSTED` — concurrent sandbox limit reached or no capacity
- `FAILED_PRECONDITION` — insufficient BOLT12 balance estimated

---

### Tool: sandbox_exec

**Description:** Execute a shell command inside a running sandbox and return the complete output synchronously. For long-running commands, use a short timeout and poll results, or use the WebSocket shell for interactive sessions.

**Input schema:**
- `sandbox_id` (string, required) — UUID of the target sandbox
- `command` (string, required) — Shell command to execute
- `timeout_ms` (integer, optional, default 30000, max 300000) — Execution timeout
- `env` (object, optional) — Additional environment variables as key-value pairs
- `working_dir` (string, optional, default `/workspace`) — Working directory inside sandbox

**Output schema:**
- `stdout` (string) — Standard output
- `stderr` (string) — Standard error
- `exit_code` (integer) — Process exit code
- `duration_ms` (integer) — Wall clock execution time
- `timed_out` (boolean) — True if command was killed by timeout

**Errors:**
- `NOT_FOUND` — Sandbox does not exist or belongs to another agent
- `FAILED_PRECONDITION` — Sandbox is not in running status
- `DEADLINE_EXCEEDED` — Command exceeded timeout_ms

---

### Tool: sandbox_write_file

**Description:** Write a file to the sandbox filesystem. Creates parent directories if they do not exist. Accepts UTF-8 text or base64-encoded binary content.

**Input schema:**
- `sandbox_id` (string, required)
- `path` (string, required) — Absolute path inside sandbox (e.g., `/workspace/script.py`)
- `content` (string, required) — File content
- `encoding` (string, optional) — `utf8` (default) or `base64`
- `create_dirs` (boolean, optional, default true) — Create parent directories

**Output schema:**
- `path` (string) — Confirmed file path
- `bytes_written` (integer) — Bytes written
- `created_dirs` (array of strings) — Directories created, if any

**Errors:**
- `NOT_FOUND` — Sandbox not found
- `FAILED_PRECONDITION` — Sandbox not running
- `INVALID_ARGUMENT` — Path is not absolute or escapes sandbox root
- `RESOURCE_EXHAUSTED` — Disk quota exceeded

---

### Tool: sandbox_read_file

**Description:** Read a file from the sandbox filesystem. Returns content as UTF-8 string or base64 for binary files.

**Input schema:**
- `sandbox_id` (string, required)
- `path` (string, required) — Absolute path inside sandbox
- `encoding` (string, optional) — `utf8` (default, fails on binary) or `base64`
- `max_bytes` (integer, optional, default 10485760) — Max bytes to read (10MB default)

**Output schema:**
- `path` (string)
- `content` (string) — File content in requested encoding
- `size_bytes` (integer) — Total file size (may exceed max_bytes)
- `truncated` (boolean) — True if file was larger than max_bytes

**Errors:**
- `NOT_FOUND` — File or sandbox not found
- `FAILED_PRECONDITION` — Sandbox not running
- `INVALID_ARGUMENT` — Path is a directory

---

### Tool: sandbox_list_files

**Description:** List files and directories at a path inside the sandbox filesystem.

**Input schema:**
- `sandbox_id` (string, required)
- `path` (string, optional, default `/workspace`) — Directory to list
- `recursive` (boolean, optional, default false) — Recurse into subdirectories
- `max_depth` (integer, optional, default 3) — Max recursion depth

**Output schema:**
- `entries` (array) — Each entry: `{name, path, type (file|directory), size_bytes, modified_at}`
- `total_entries` (integer)

---

### Tool: sandbox_status

**Description:** Get the current status, resource usage, and billing information for a sandbox.

**Input schema:**
- `sandbox_id` (string, required)

**Output schema:**
- `sandbox_id` (string)
- `status` (string) — pending, running, paused, terminated
- `tier` (string)
- `runtime` (string)
- `uptime_secs` (integer)
- `sats_consumed` (integer) — Total sats billed so far
- `sats_per_second` (number) — Current billing rate
- `cpu_percent` (number) — Current CPU utilization
- `memory_used_mb` (integer)
- `memory_limit_mb` (integer)
- `disk_used_mb` (integer)
- `template` (string)
- `created_at` (string)
- `last_billed_at` (string)
- `warning_level` (string) — ok, warning, critical, suspended

---

### Tool: sandbox_pause

**Description:** Pause a running sandbox by snapshotting its complete state to cloud storage and stopping billing. The sandbox can be resumed later. State is retained for 24 hours.

**Input schema:**
- `sandbox_id` (string, required)

**Output schema:**
- `sandbox_id` (string)
- `status` (string) — `paused`
- `snapshot_gcs` (string) — GCS path to the snapshot
- `snapshot_size_mb` (integer)
- `sats_consumed_total` (integer)
- `paused_at` (string)
- `snapshot_expires_at` (string) — 24 hours after pause

---

### Tool: sandbox_resume

**Description:** Resume a paused sandbox by restoring its state from the snapshot. Billing restarts immediately. Returns new WebSocket credentials.

**Input schema:**
- `sandbox_id` (string, required)

**Output schema:**
- `sandbox_id` (string)
- `status` (string) — `running`
- `wss_url` (string) — New WebSocket URL
- `wss_token` (string) — Refreshed token
- `resumed_at` (string)

**Errors:**
- `NOT_FOUND` — Sandbox not found or not paused
- `RESOURCE_EXHAUSTED` — Snapshot has expired (> 24h)

---

### Tool: sandbox_terminate

**Description:** Permanently terminate a sandbox. All data inside the sandbox is destroyed. Billing stops immediately. This action cannot be undone.

**Input schema:**
- `sandbox_id` (string, required)
- `confirm` (boolean, required) — Must be true to proceed (safety check for LLMs)

**Output schema:**
- `sandbox_id` (string)
- `status` (string) — `terminated`
- `total_uptime_secs` (integer)
- `total_sats_consumed` (integer)
- `terminated_at` (string)

---

### Tool: wallet_balance

**Description:** Get the current Lightning wallet balance and active sandbox summary for the authenticated agent.

**Input schema:** (none)

**Output schema:**
- `bolt12_offer` (string) — Agent's registered BOLT12 offer
- `offer_type` (string) — bolt12, lnurl, or lightning_address
- `active_sandboxes` (integer) — Number of currently running sandboxes
- `active_sats_per_second` (number) — Total billing rate across all running sandboxes
- `total_sats_spent_lifetime` (integer)
- `warning_level` (string) — ok, warning, critical, suspended

---

### Tool: sandbox_delegate

**Description:** Create an attenuated macaroon credential that grants limited access to a specific sandbox. Use this to delegate sandbox access to sub-agents with restricted permissions.

**Input schema:**
- `sandbox_id` (string, required)
- `readonly` (boolean, optional) — If true, delegate can only read files and status; cannot exec or write
- `expiry_secs` (integer, optional) — Credential expiry in seconds from now (default 3600)
- `max_exec_calls` (integer, optional) — Maximum number of exec calls allowed

**Output schema:**
- `delegated_token` (string) — Base64-encoded attenuated macaroon
- `caveats` (object) — Summary of restrictions applied
- `expires_at` (string) — ISO8601 expiry timestamp

---

## MCP Resources

In addition to tools, the MCP server exposes read-only resources that LLMs can reference.

### Resource: sandbox://{sandbox_id}/status

Returns the same data as `sandbox_status` tool but in MCP Resource format.

- URI template: `sandbox://{sandbox_id}/status`
- MIME type: `application/json`
- Updated every 5 seconds (LLM can subscribe to updates)

### Resource: agent://billing/history

Returns the last 20 billing intervals for the authenticated agent.

- URI: `agent://billing/history`
- MIME type: `application/json`

### Resource: platform://pricing

Returns current tier pricing in sats/second.

- URI: `platform://pricing`
- MIME type: `application/json`
- Updated: on pricing changes only

---

## MCP Client Configuration Examples

### Claude Desktop (claude_desktop_config.json)

```
{
  "mcpServers": {
    "satoshi-compute": {
      "command": "npx",
      "args": ["-y", "@satoshi-compute/mcp-proxy"],
      "env": {
        "SATOSHI_API_URL": "https://api.satoshicompute.io",
        "AGENT_JWT": "eyJ...",
        "LEXE_SEED_PATH": "~/.lexe/seedphrase.txt"
      }
    }
  }
}
```

The `@satoshi-compute/mcp-proxy` npm package is a thin Node.js process that:
- Holds the agent JWT
- Translates stdio MCP protocol to HTTP SSE transport
- Handles BOLT12 payment for L402 if the JWT has expired and needs refresh

### Direct HTTP SSE (for agents with HTTP clients)

```
GET https://api.satoshicompute.io/mcp/sse
Authorization: Bearer {agent_jwt}
Accept: text/event-stream
```

---

## Error Handling in MCP Context

MCP errors are returned in JSON-RPC 2.0 error format:

```
{
  "jsonrpc": "2.0",
  "id": "call_123",
  "error": {
    "code": -32602,
    "message": "Sandbox not found",
    "data": {
      "error_slug": "sandbox_not_found",
      "sandbox_id": "uuid",
      "request_id": "req_uuid"
    }
  }
}
```

### MCP Error Code Mapping

| Go API Error | JSON-RPC Code | MCP Meaning |
|---|---|---|
| 400 invalid params | -32602 | Invalid params |
| 401 unauthorized | -32001 | Auth error (custom) |
| 402 payment required | -32002 | Payment required (custom) |
| 404 not found | -32003 | Not found (custom) |
| 409 conflict | -32004 | Conflict (custom) |
| 429 rate limited | -32005 | Rate limited (custom) |
| 500 internal error | -32603 | Internal error |
| 503 unavailable | -32006 | Service unavailable (custom) |

---

## MCP Server Lifecycle

### Session Management

Each SSE connection is a session. Sessions are tracked in-memory by session_id:
- Session created on `GET /mcp/sse`
- Session destroyed on SSE connection close or 30-minute inactivity timeout
- Maximum concurrent sessions per agent: 5 (to prevent LLM loops from exhausting connections)
- Session state: authenticated agent_id, active tool calls, pending responses

### Concurrency

MCP tool calls within a session are serialized by default (LLMs typically send one tool call at a time). If the client sends multiple concurrent tool calls, they are processed in parallel with individual goroutines and responses are matched by JSON-RPC `id`.

---

## Monitoring

| Metric | Type | Labels |
|---|---|---|
| `mcp_sessions_active` | Gauge | — |
| `mcp_tool_calls_total` | Counter | tool_name, status |
| `mcp_tool_call_duration_ms` | Histogram | tool_name |
| `mcp_session_duration_secs` | Histogram | — |
| `mcp_errors_total` | Counter | error_code, tool_name |

---

## Dependencies

- **api module** — all tool handlers delegate to the same functions called by REST handlers
- **Supabase Auth** — JWT validation for MCP session authentication
- **billing module** — sandbox_create triggers billing start; sandbox_terminate/pause stops billing
