# Component 7: MCP Server

## v2 changelog

- Transport switched from HTTP+SSE to **MCP Streamable HTTP** (the 2025-11 transport refresh): one bidirectional HTTP/2 stream per session, not separate GET (SSE) + POST (message) channels. Halves connection count, simplifies session management, and works cleanly through the GCP global LB without SSE-specific quirks.
- Authentication is **JWT-only** (Supabase user JWT or platform-issued L402-derived JWT). The MCP `tools/list` and `tools/call` endpoints never trigger an L402 402 cycle. L402 happens once during agent registration; thereafter the JWT carries the identity.
- MCP sessions are pinned to a control-plane instance via **consistent hashing of `session_id`**. Requests for an existing session that hit the wrong instance are forwarded over internal gRPC by `OwnershipForwardMiddleware`.
- Tools that require data-plane access (`sandbox_exec`, `sandbox_write_file`, `sandbox_read_file`, `sandbox_list_files`) are **proxied by the MCP server to the host-agent's HTTPS endpoint** using the sandbox's `data_jwt` minted at sandbox creation. The MCP server does NOT call exec/files via the control-plane orchestrator (which no longer exposes those operations).
- Output schemas use msats (Lightning) and cents-x100 (credits), matching the Component 3 / Component 6 unit changes. The `sats_per_second` and `sats_consumed` fields are kept as derived integer values for backward compatibility with existing clients but accompanied by the higher-precision `msats_per_second` and `msats_consumed`.

---

## Overview

The MCP (Model Context Protocol) Server is the `internal/mcp` package within the **control-plane binary**. It exposes Sockt sandbox operations as MCP-compliant tools, allowing any MCP-compatible LLM — Claude, Gemini, GPT-4 — to provision, operate, and pay for compute sandboxes natively.

Tool calls route to the same handlers used by the REST API where the operation is a control-plane operation (create, pause, resume, terminate, status). For data-plane operations (`exec`, file read/write/list), the MCP server proxies the call to the appropriate host-agent's `/h/{host_id}/sandbox/{id}/...` HTTPS endpoint using the sandbox's `data_jwt`. From the LLM's perspective, all tools "just work" — it never sees the data-plane URL.

---

## MCP Protocol Version

Implements the **MCP 2025-11 specification** with the **Streamable HTTP transport**.

- One HTTP/2 bidirectional stream per session (no SSE; no separate POST channel)
- JSON-RPC 2.0 framing on the stream
- Tool definitions: fully typed JSON Schema for inputs and outputs
- Resource exposure: sandbox status and billing history exposed as MCP Resources
- No sampling or completion features used (tools only)

---

## Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/mcp/stream` | Open a Streamable HTTP session. Returns `Mcp-Session-Id` header on success and keeps the response stream open for server→client messages. Subsequent client→server messages are sent as additional POSTs that the server matches by `Mcp-Session-Id`. |
| `GET` | `/mcp/health` | MCP server health check. Returns `{"status":"ok","protocol_version":"2025-11"}` |

Note: legacy `/mcp/sse` and `/mcp/message` endpoints are no longer supported. The legacy `@sockt/mcp-proxy` package is updated in lockstep to use the streamable transport.

### Authentication

MCP connections require a valid JWT in the `Authorization: Bearer {jwt}` header on both the initial POST and on every subsequent message POST in the same session. Two JWT kinds are accepted:
- **Supabase user JWT** — for website users who use API-key billing
- **Agent JWT** — issued at L402 registration; signed by the platform's HS256 secret

Both are validated by the same `JWTMiddleware` (Component 1). The MCP server does NOT issue L402 challenges.

The underlying tool calls (e.g., `sandbox_create`) trigger BOLT12 top-ups automatically via the billing engine — the LLM does not handle payment logic directly.

### Session Affinity

When a new session begins (`initialize` message), the MCP server creates a session record keyed by a fresh `session_id` (UUID). The `Mcp-Session-Id` response header pins subsequent requests to that session.

The MCP server uses `cluster.OwnerOf(session_id)` to decide where the session lives. If a request for an existing session arrives at a non-owner instance, `OwnershipForwardMiddleware` forwards it over internal gRPC. This means the GCP LB can hash to any instance and the session still works.

---

## MCP Server Initialization Sequence

1. Client opens `POST /mcp/stream` with `Authorization: Bearer {jwt}`
2. Server validates the JWT, mints a session_id, and pins ownership via the consistent-hash ring
3. Server holds the response stream open and writes its `initialize` response
4. Client writes its `initialize` message body: `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-11","capabilities":{},"clientInfo":{"name":"claude","version":"3.7"}}}`
5. Server responds with capabilities + serverInfo and the `Mcp-Session-Id` header
6. Client sends `tools/list` — server returns all tool definitions
7. Client calls tools via `tools/call` messages

The session is destroyed on stream close, on 30 minutes of inactivity, or when the owning instance leaves the cluster (the new owner's session map starts empty; the client must re-initialize).

---

## Tool Definitions

### Tool: `sandbox_create`

**Description:** Provision a new compute sandbox environment. Automatically handles initial top-up payment for the session. Returns credentials to interact with the sandbox.

**Input schema:**
- `tier` (string, required) — Enum: `nano`, `micro`, `standard`, `gpu_small`, `gpu_large`
- `template` (string, optional) — Enum: `ubuntu-base`, `python3`, `nodejs`, `rust`, `ml-cuda`
- `duration_secs` (integer, optional) — Estimated session length; controls initial top-up amount
- `egress` (boolean, optional) — Enable outbound internet access
- `label` (string, optional) — Human label for this sandbox
- `idempotency_key` (string, optional) — Caller UUID for safe retry

**Output schema:**
- `sandbox_id` (string)
- `status` (string) — `pending` or `running`
- `tier` (string)
- `runtime` (string) — `firecracker` or `dedicated_vm`
- `template` (string)
- `data_endpoint` (string) — Host-data-plane base URL (host-agent HTTPS endpoint behind LB)
- `host_id` (string) — The host VM serving this sandbox
- `sats_per_second` (number)
- `msats_per_second` (integer)
- `initial_balance_sats` (integer)
- `created_at` (string) — ISO8601
- `estimated_ready_at` (string) — ISO8601 (may be future for dedicated VMs)

**MCP-relevant errors:**
- `INVALID_PARAMS` — unknown tier/template
- `RESOURCE_EXHAUSTED` — concurrent sandbox limit or no capacity
- `FAILED_PRECONDITION` — initial top-up failed (BOLT12 routing failure or insufficient credit balance)

(`wss_token` is no longer in the tool output. The SDK can construct the WSS URL as `data_endpoint + "/shell?token=" + data_jwt`. The full `data_jwt` is available via `sandbox_get_data_jwt` if the LLM needs it, though normally only the SDK uses it.)

---

### Tool: `sandbox_exec`

**Description:** Execute a shell command inside a running sandbox and return the complete output synchronously. The MCP server transparently proxies this call to the sandbox's host-agent — latency is dominated by the actual exec, not by network hops.

**Input schema:**
- `sandbox_id` (string, required)
- `command` (string, required)
- `timeout_ms` (integer, optional, default 30000, max 300000)
- `env` (object, optional)
- `working_dir` (string, optional, default `/workspace`)

**Output schema:**
- `stdout` (string)
- `stderr` (string)
- `exit_code` (integer)
- `duration_ms` (integer)
- `timed_out` (boolean)

**Implementation:** the MCP server looks up `sandboxes.host_vm_id`, mints a fresh `data_jwt` (with `scope=exec`), and POSTs to `https://api.sockt.dev/h/{host_id}/sandbox/{id}/exec` with the JWT. The response is forwarded to the LLM as the tool result.

**Errors:**
- `NOT_FOUND` — Sandbox does not exist or belongs to another agent/user
- `FAILED_PRECONDITION` — Sandbox is not in running status
- `DEADLINE_EXCEEDED` — Command exceeded `timeout_ms`

---

### Tool: `sandbox_write_file`

**Description:** Write a file to the sandbox filesystem.

**Input schema:** `sandbox_id`, `path`, `content`, `encoding` (`utf8` or `base64`), `create_dirs`

**Output schema:** `path`, `bytes_written`, `created_dirs`

**Implementation:** Proxied to host-agent's `POST /h/{host_id}/sandbox/{id}/files` with `data_jwt` (scope=files).

---

### Tool: `sandbox_read_file`

**Description:** Read a file from the sandbox filesystem.

**Input schema:** `sandbox_id`, `path`, `encoding`, `max_bytes`

**Output schema:** `path`, `content`, `size_bytes`, `truncated`

**Implementation:** Proxied to host-agent's `GET /h/{host_id}/sandbox/{id}/files`.

---

### Tool: `sandbox_list_files`

**Description:** List files and directories at a path inside the sandbox.

**Input schema:** `sandbox_id`, `path`, `recursive`, `max_depth`

**Output schema:** `entries` (array), `total_entries`

**Implementation:** Proxied to host-agent's `GET /h/{host_id}/sandbox/{id}/files/list`.

---

### Tool: `sandbox_status`

**Description:** Get the current status, resource usage, and prepaid balance for a sandbox.

**Input schema:** `sandbox_id`

**Output schema:**
- `sandbox_id` (string)
- `status` (string)
- `tier` (string)
- `runtime` (string)
- `uptime_secs` (integer)
- `balance_msats` (integer) — current prepaid Lightning balance (Lightning sandboxes)
- `balance_usd_cents` (integer) — current local credit allocation (credits sandboxes)
- `consumed_msats` (integer) — lifetime Lightning consumption
- `consumed_usd_cents` (integer) — lifetime credit consumption
- `sats_per_second` (number)
- `cpu_percent` (number)
- `memory_used_mb` (integer)
- `memory_limit_mb` (integer)
- `disk_used_mb` (integer)
- `template` (string)
- `host_id` (string)
- `created_at` (string)
- `last_metered_at` (string)
- `warning_level` (string) — `ok`, `low_balance`, `out_of_balance`, `suspended` (Lightning) or `ok`, `low_credits`, `out_of_credits`, `suspended` (credits)

**Implementation:** The MCP server merges control-plane state (status, host_id, billing rate) with live host-agent metrics (CPU/mem/disk) by also calling `GET /h/{host_id}/sandbox/{id}/status`. If the host-agent is briefly unreachable, returns the control-plane snapshot only and sets `metrics_stale: true`.

---

### Tool: `sandbox_pause`

**Description:** Pause a running sandbox by snapshotting its complete state to host-local SSD (and asynchronously to GCS). Stops billing. Snapshot retained for 24 hours.

**Output schema:**
- `sandbox_id`, `status` (= `paused`)
- `snapshot_storage` (= `host_local_ssd`)
- `snapshot_size_mb`
- `consumed_msats_total` (integer)
- `paused_at`, `snapshot_expires_at`

---

### Tool: `sandbox_resume`

**Description:** Resume a paused sandbox by restoring its state. Hot resume on the original host (sub-second) when possible; otherwise cold resume from GCS.

**Output schema:**
- `sandbox_id`, `status` (= `running`)
- `data_endpoint` (string) — refreshed (host may have changed)
- `host_id` (string)
- `resumed_at`, `resumed_from` (`host_local_ssd` or `gcs`)

**Errors:**
- `NOT_FOUND` — sandbox not found or not paused
- `RESOURCE_EXHAUSTED` — snapshot has expired (> 24h)

---

### Tool: `sandbox_terminate`

**Description:** Permanently terminate a sandbox. All data inside is destroyed. Billing stops immediately. Cannot be undone.

**Input schema:** `sandbox_id`, `confirm` (boolean, must be true)

**Output schema:** `sandbox_id`, `status` (= `terminated`), `total_uptime_secs`, `consumed_msats_total`, `terminated_at`

---

### Tool: `wallet_balance`

**Description:** Get the current Lightning prepaid balance and active sandbox summary for the authenticated agent.

**Input schema:** (none)

**Output schema:**
- `bolt12_offer` (string) — The agent's registered offer
- `offer_type` (string) — `bolt12` or `manual_bolt11`
- `balance_msats` (integer) — Current pre-funded balance held by the platform on behalf of the agent
- `active_sandboxes` (integer)
- `active_msats_per_second` (integer)
- `total_msats_topped_up_lifetime` (integer)
- `total_msats_consumed_lifetime` (integer)
- `warning_level` (string)

For credit users, `balance_usd_cents` etc. are returned analogously from a `credit_balance` tool (separate tool to keep schemas clean).

---

### Tool: `sandbox_extend_balance`

**Description:** Manually trigger a top-up of additional sats for a sandbox. Useful before a known-large workload to avoid mid-run top-up latency.

**Input schema:** `sandbox_id`, `sats` (integer, required), `idempotency_key` (string, optional)

**Output schema:** `balance_msats_after`, `topup_settled_at`, `preimage`

---

### Tool: `sandbox_delegate`

**Description:** Create an attenuated macaroon credential that grants limited access to a specific sandbox. Use this to delegate sandbox access to sub-agents with restricted permissions.

**Input schema:**
- `sandbox_id` (string, required)
- `readonly` (boolean, optional)
- `expiry_secs` (integer, optional, default 3600)
- `max_exec_calls` (integer, optional)

**Output schema:**
- `delegated_token` (string) — Base64-encoded attenuated macaroon
- `caveats` (object)
- `expires_at` (string)

---

## MCP Resources

In addition to tools, the MCP server exposes read-only resources.

### Resource: `sandbox://{sandbox_id}/status`

Same data as `sandbox_status` tool but in MCP Resource format.
- URI template: `sandbox://{sandbox_id}/status`
- MIME type: `application/json`
- Updated every 5 seconds; clients can subscribe

### Resource: `agent://billing/history`

Last 20 billing intervals for the authenticated agent (Lightning) or last 20 credit transactions (credits).
- URI: `agent://billing/history`
- MIME type: `application/json`

### Resource: `platform://pricing`

Current tier pricing in msats/second and usd-cents/second.
- URI: `platform://pricing`
- MIME type: `application/json`
- Updated only on pricing changes (we publish via `tier_pricing_changed` LISTEN/NOTIFY)

---

## MCP Client Configuration Examples

### Claude Desktop (claude_desktop_config.json) — API key path

```json
{
  "mcpServers": {
    "sockt": {
      "command": "npx",
      "args": ["-y", "@sockt/mcp-server"],
      "env": {
        "SOCKT_API_URL": "https://api.sockt.dev",
        "SOCKT_API_KEY": "sockt_live_aB3xK9mPqR7vNwYzL2cD5eF8gH1jI4k"
      }
    }
  }
}
```

### Claude Desktop — L402 + Lexe path

```json
{
  "mcpServers": {
    "sockt": {
      "command": "npx",
      "args": ["-y", "@sockt/mcp-server"],
      "env": {
        "SOCKT_API_URL": "https://api.sockt.dev",
        "AGENT_JWT": "eyJ...",
        "LEXE_SEED_PATH": "~/.lexe/seedphrase.txt"
      }
    }
  }
}
```

The `@sockt/mcp-server` npm package is a thin Node.js process that:
- Holds the agent JWT (or fetches one via L402 + Lexe at startup)
- Translates stdio MCP protocol to the Streamable HTTP transport
- Handles JWT refresh transparently

### Direct MCP Streamable HTTP (for native MCP clients)

```
POST https://api.sockt.dev/mcp/stream
Authorization: Bearer {jwt}
Content-Type: application/json
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

| Source Error | JSON-RPC Code | MCP Meaning |
|---|---|---|
| 400 invalid params | -32602 | Invalid params |
| 401 unauthorized | -32001 | Auth error (custom) |
| 402 topup_failed / insufficient_credits | -32002 | Payment required (custom) |
| 404 not found | -32003 | Not found (custom) |
| 409 conflict | -32004 | Conflict (custom) |
| 429 rate limited | -32005 | Rate limited (custom) |
| 500 internal error | -32603 | Internal error |
| 503 host_unreachable / unavailable | -32006 | Service unavailable (custom) |

---

## MCP Server Lifecycle

### Session Management

Each Streamable HTTP connection is a session, owned by a single control-plane instance via consistent hashing. The session struct holds:
- `session_id` (UUID)
- `principal` (the auth result)
- `client_info` (from `initialize`)
- `pending_tool_calls` (map keyed by JSON-RPC `id`)
- `data_jwt_cache` (per-sandbox: avoid re-minting on every tool call)
- `created_at`, `last_active_at`

Sessions expire after 30 minutes of inactivity. Maximum concurrent sessions per principal: 5.

### Concurrency

MCP tool calls within a session are processed in parallel via individual goroutines; responses are matched by JSON-RPC `id`. The data-plane proxy calls (sandbox_exec etc.) use a per-host-agent gRPC channel pool for outbound calls to avoid head-of-line blocking.

### Cluster Migration

When the consistent-hash ring rebalances, sessions whose ownership moves are NOT migrated — the new owner's session map starts empty. Existing client sessions on the moved-away instance receive a `session_migrated` JSON-RPC error and re-initialize, which the MCP proxy package handles transparently. Session loss is bounded to the few seconds of in-flight tool calls during a rebalance.

---

## Monitoring

| Metric | Type | Labels |
|---|---|---|
| `mcp_sessions_active` | Gauge | — (per instance) |
| `mcp_tool_calls_total` | Counter | tool_name, status |
| `mcp_tool_call_duration_ms` | Histogram | tool_name |
| `mcp_data_plane_proxy_duration_ms` | Histogram | tool_name |
| `mcp_data_plane_proxy_errors_total` | Counter | tool_name, error_class |
| `mcp_session_duration_secs` | Histogram | — |
| `mcp_errors_total` | Counter | error_code, tool_name |

---

## Dependencies

- **api module** — control-plane tool handlers (create/pause/resume/terminate/status/delegate/extend_balance/wallet_balance) delegate to the same functions called by REST handlers
- **Auth module** (Component 1) — JWT validation; mints `data_jwt` for proxied data-plane calls
- **Cluster module** (Component 2 / `internal/cluster`) — session affinity via consistent hashing
- **Host-agent HTTPS data plane** (Components 2, 4, 8) — destination of proxied exec/files/list calls
- **Billing module** (Component 3) — `sandbox_create` triggers initial top-up; `sandbox_terminate`/`pause` calls `DeregisterSandbox`; `sandbox_extend_balance` triggers a manual top-up
