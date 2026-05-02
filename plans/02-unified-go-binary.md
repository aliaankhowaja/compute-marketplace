# Component 2: Unified Go Binary

## Overview

The unified Go binary is the core of Satoshi Compute's control plane. It merges what would traditionally be three separate services — the REST API, billing middleware, and sandbox orchestrator — into a single deployable binary structured as a **modular monolith**. Modules communicate via in-process function calls and shared channels, eliminating the inter-service HTTP latency that would otherwise dominate billing tick performance.

The binary is deployed on GCP Cloud Run alongside the Aperture sidecar. It manages all sandbox lifecycle state, drives billing tickers, proxies WebSocket shell sessions, and serves the MCP tool interface.

---

## Design Philosophy

### Why a Modular Monolith

A microservice architecture would require the billing engine to make an HTTP call to the orchestrator on every billing tick (up to 10,000 calls/sec at scale). With in-process calls, billing ticks resolve in microseconds rather than milliseconds. Each module has:

- Its own Go package under `internal/`
- Its own interface definition that other modules depend on (no direct struct coupling)
- Its own unit tests with mocked dependencies
- A clearly owned subset of the Supabase schema

Modules can be extracted into separate services if a single module becomes a scaling bottleneck, without rewriting anything — just expose the existing interface over gRPC.

---

## Repository Structure

```
satoshi-compute/
├── cmd/
│   └── server/
│       └── main.go                  # Entry point: wires all modules, starts servers
├── internal/
│   ├── api/                         # HTTP handler layer
│   │   ├── handler_sandbox.go
│   │   ├── handler_wallet.go
│   │   ├── handler_billing.go
│   │   ├── handler_discovery.go
│   │   ├── middleware_auth.go       # Strips + validates Aperture trust headers
│   │   ├── middleware_logging.go
│   │   ├── middleware_recovery.go
│   │   └── router.go                # chi router wiring
│   ├── billing/                     # Streaming payment billing engine
│   │   ├── billing.go               # BillingEngine struct + interface
│   │   ├── ticker.go                # Per-sandbox goroutine ticker
│   │   └── budget.go                # Budget warning + pause-on-failure logic
│   ├── orchestrator/                # VM lifecycle manager
│   │   ├── orchestrator.go          # Orchestrator struct + interface
│   │   ├── firecracker.go           # Firecracker VMM client (Unix socket)
│   │   ├── gpu.go                   # Dedicated VM provisioning via GCP API
│   │   ├── host_pool.go             # In-memory host capacity tracking
│   │   ├── router.go                # standard-tier Firecracker vs dedicated routing
│   │   └── snapshot.go              # Pause/resume snapshot logic
│   ├── payment/                     # Lightning / L402 client
│   │   ├── payment.go               # Payment interface
│   │   ├── lnd.go                   # LND gRPC client implementation
│   │   └── bolt12.go                # BOLT12 offer + invoice_request logic
│   ├── store/                       # Supabase Postgres adapter
│   │   ├── store.go                 # Store interface
│   │   ├── agents.go
│   │   ├── sandboxes.go
│   │   ├── billing.go
│   │   └── hosts.go
│   ├── mcp/                         # MCP server
│   │   ├── server.go
│   │   └── tools.go
│   ├── wss/                         # WebSocket shell server
│   │   ├── server.go
│   │   └── proxy.go                 # virtio-vsock / SSH proxy
│   └── config/                      # Configuration loading from env + Secret Manager
│       └── config.go
├── migrations/                      # SQL migration files (applied via Supabase CLI)
├── Dockerfile
├── cloudbuild.yaml
└── openapi.yaml                     # Auto-generated OpenAPI 3.0 spec
```

---

## Module Interfaces

Each module is depended upon through an interface, not a concrete struct. This enables clean mocking in tests and future service extraction.

### Store Interface

Defines all database operations. Implemented by the Supabase Postgres adapter.

**Agent operations:**
- `CreateAgent(ctx, bolt12Offer, jwtSub) → Agent`
- `GetAgentByJWT(ctx, jwtSub) → Agent`
- `GetAgentByOffer(ctx, bolt12Offer) → Agent`
- `UpdateAgentBalance(ctx, agentID, deltaSats) → error`

**Sandbox operations:**
- `CreateSandbox(ctx, SandboxCreate) → Sandbox`
- `GetSandbox(ctx, sandboxID) → Sandbox`
- `GetSandboxesByAgent(ctx, agentID, filter) → []Sandbox`
- `UpdateSandboxStatus(ctx, sandboxID, status) → error`
- `UpdateSandboxHost(ctx, sandboxID, hostVMID, guestIP) → error`
- `SetSandboxSnapshot(ctx, sandboxID, gcsPath) → error`
- `TerminateSandbox(ctx, sandboxID) → error`
- `IncrementSandboxSats(ctx, sandboxID, sats) → error`

**Billing operations:**
- `RecordBillingInterval(ctx, BillingInterval) → error`
- `GetBillingHistory(ctx, agentID, page, limit) → []BillingInterval`
- `GetTotalSatsConsumed(ctx, sandboxID) → int64`

**Host VM operations:**
- `GetHostsByAvailableMemMB(ctx, requiredMB) → []HostVM`
- `AllocateHostMemory(ctx, hostID, memMB) → error`
- `DeallocateHostMemory(ctx, hostID, memMB) → error`
- `UpsertHostVM(ctx, HostVM) → error`
- `UpdateHostHeartbeat(ctx, hostID) → error`
- `GetAllHosts(ctx) → []HostVM`

### Orchestrator Interface

- `CreateSandbox(ctx, CreateRequest) → Sandbox`
- `TerminateSandbox(ctx, sandboxID) → error`
- `PauseSandbox(ctx, sandboxID) → SnapshotResult`
- `ResumeSandbox(ctx, sandboxID) → ResumeResult`
- `ExecCommand(ctx, sandboxID, command, timeoutMS) → ExecResult`
- `WriteFile(ctx, sandboxID, path, content, encoding) → WriteResult`
- `ReadFile(ctx, sandboxID, path) → ReadResult`
- `GetSandboxMetrics(ctx, sandboxID) → VMMetrics`
- `RefreshHostPool(ctx) → error`

### Billing Interface

- `StartBilling(ctx, sandbox) → error`
- `StopBilling(sandboxID) → error`
- `GetActiveTickers() → []TickerStatus`
- `ForceSettle(ctx, sandboxID) → SettleResult`

### Payment Interface

- `RegisterOffer(ctx, bolt12Offer) → AgentToken`
- `RequestBOLT12Invoice(ctx, offer, sats, memo) → Invoice`
- `PayInvoice(ctx, invoice) → Preimage`
- `GetNodeBalance(ctx) → BalanceInfo`
- `CreateDepositInvoice(ctx, sats) → BOLT11Invoice`
- `VerifyPreimage(preimage, paymentHash) → bool`

---

## Startup Sequence

The `main.go` entry point wires all modules and starts servers in dependency order:

1. **Load config** from environment variables and GCP Secret Manager
2. **Initialize store** — create pgxpool connection to Supabase, run connectivity check
3. **Initialize payment** — connect to LND gRPC, verify node is synced, retrieve BOLT12 support status
4. **Initialize orchestrator** — load host VM pool from Supabase `host_vms` table into memory, connect to GCP Compute Engine API
5. **Initialize billing** — wire store + payment + orchestrator; recover any interrupted billing tickers for `status=running` sandboxes (crash recovery)
6. **Initialize MCP server** — wire api interface; start MCP SSE listener on internal port
7. **Initialize WebSocket server** — wire orchestrator; start WSS listener on internal port
8. **Initialize HTTP API** — wire all modules; mount chi router; mount Prometheus metrics endpoint
9. **Start goroutines**:
   - Billing engine ticker loop
   - Host pool heartbeat refresher (every 30 seconds)
   - Graceful shutdown listener (SIGTERM)
10. **Start HTTP server** on `0.0.0.0:8081` (Aperture forwards to this port)

### Crash Recovery on Startup

On startup, the billing module queries Supabase for all sandboxes with `status = 'running'`. For each, it restarts the billing ticker goroutine. This ensures no sandbox is silently left running without billing after a Cloud Run instance restart.

---

## HTTP Router — All Routes

All routes are mounted on the chi router inside the `api` module. Routes prefixed `/v1/` pass through Aperture before reaching Go handlers. Routes listed as `(public)` are explicitly excluded from Aperture's gating in the Aperture service config.

### Middleware Stack (applied to all /v1/ routes)

1. `RequestIDMiddleware` — generates or propagates X-Request-ID
2. `StructuredLoggingMiddleware` — logs method, path, agent_id, duration_ms, status
3. `RecoveryMiddleware` — catches panics, returns 500 JSON
4. `ApertureHeaderTrustMiddleware` — validates X-Macaroon-Verified header; extracts agent_id from X-Agent-ID
5. `ContentTypeMiddleware` — enforces application/json for POST/PUT bodies

### Route Table

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/healthz` | `HandleHealthCheck` | Public |
| `GET` | `/metrics` | `HandlePrometheusMetrics` | Public (Cloud-internal only via VPC firewall) |
| `GET` | `/v1/pricing` | `HandleGetPricing` | Public |
| `GET` | `/v1/templates` | `HandleListTemplates` | Public |
| `POST` | `/v1/wallet/offer` | `HandleRegisterOffer` | L402 (1 sat) |
| `GET` | `/v1/wallet/balance` | `HandleGetBalance` | L402 + JWT |
| `POST` | `/v1/wallet/topup` | `HandleTopup` | L402 + JWT |
| `POST` | `/v1/sandbox/create` | `HandleCreateSandbox` | L402 + JWT |
| `GET` | `/v1/sandbox/{id}` | `HandleGetSandbox` | L402 + JWT + sandbox_id caveat |
| `GET` | `/v1/sandbox/{id}/status` | `HandleGetSandboxStatus` | L402 + JWT + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/exec` | `HandleExec` | L402 + JWT + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/files` | `HandleWriteFile` | L402 + JWT + sandbox_id caveat |
| `GET` | `/v1/sandbox/{id}/files` | `HandleReadFile` | L402 + JWT + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/pause` | `HandlePause` | L402 + JWT + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/resume` | `HandleResume` | L402 + JWT + sandbox_id caveat |
| `DELETE` | `/v1/sandbox/{id}` | `HandleTerminate` | L402 + JWT + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/delegate` | `HandleDelegate` | L402 + JWT + sandbox_id caveat |
| `GET` | `/v1/billing/history` | `HandleBillingHistory` | L402 + JWT |
| `GET` | `/v1/sandbox/{id}/billing` | `HandleSandboxBilling` | L402 + JWT + sandbox_id caveat |
| `GET` | `/v1/mcp/sse` | `MCPSSEHandler` | JWT only (no L402; connection established post-registration) |
| `GET` | `/v1/sandbox/{id}/shell` (WSS upgrade) | `WSSUpgradeHandler` | JWT + wss_token query param |

---

## Handler Specifications

### POST /v1/wallet/offer — HandleRegisterOffer

**Purpose:** First endpoint an agent calls. Registers their BOLT12 offer with the platform and receives a Supabase JWT for subsequent authenticated calls.

**Request body:**
- `bolt12_offer` (string, required) — agent's BOLT12 offer string from their Lexe wallet
- `label` (string, optional) — human label for this agent identity

**Validation:**
- `bolt12_offer` must be a valid BOLT12 offer (prefix `lno1` for mainnet)
- Must not already be registered (unique constraint in Supabase)

**Processing:**
1. Call `payment.ValidateOffer(bolt12_offer)` — verifies offer is decodable
2. Call `store.CreateAgent(bolt12_offer)` — creates agent row
3. Call Supabase Auth Admin API to create user with `id = agent.id`
4. Sign and return Supabase JWT

**Response 200:**
```
{
  "agent_id": "uuid",
  "agent_token": "supabase_jwt",
  "token_expires_at": "ISO8601",
  "message": "Offer registered. Include X-Agent-Token header on future requests."
}
```

**Error cases:**
- `400 offer_invalid` — BOLT12 offer string is malformed
- `409 offer_already_registered` — offer already exists

---

### POST /v1/sandbox/create — HandleCreateSandbox

**Purpose:** Provision a new sandbox environment. Triggers VM creation and starts billing ticker.

**Request body:**
- `tier` (string, required) — `nano | micro | standard | gpu_small | gpu_large`
- `template` (string, optional) — `python3 | nodejs | rust | ubuntu-base | ml-cuda`
- `duration_secs` (integer, optional) — budget hint; encoded as macaroon caveat
- `egress` (boolean, optional, default false) — enable outbound internet (triggers additional L402)
- `label` (string, optional) — agent-assigned label for this sandbox

**Validation:**
- `tier` must be a known value
- Agent must have sufficient BOLT12 offer balance (pre-flight check: estimate cost = tier.sats_per_second × duration_secs)
- Concurrent sandbox limit not exceeded (checked against Supabase)

**Processing:**
1. Create sandbox row in Supabase with `status = 'pending'`
2. Call `orchestrator.CreateSandbox(tier, template)` — returns host_vm_id, guest_ip, wss_token
3. Update sandbox row: `status = 'running'`, `host_vm_id`, `guest_ip`, `wss_token`
4. Call `billing.StartBilling(sandbox)` — spawns ticker goroutine
5. Return sandbox credentials

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "running",
  "tier": "micro",
  "runtime": "firecracker",
  "template": "python3",
  "wss_url": "wss://api.satoshicompute.io/v1/sandbox/{id}/shell",
  "wss_token": "string",
  "rest_base": "https://api.satoshicompute.io/v1/sandbox/{id}",
  "sats_per_second": 2.0,
  "host_vm": "vm-host-pool-001",
  "created_at": "ISO8601",
  "estimated_ready_at": "ISO8601"
}
```

**Error cases:**
- `400 invalid_tier`
- `400 invalid_template`
- `402 insufficient_balance` — agent's BOLT12 offer cannot cover estimated cost
- `429 sandbox_limit_exceeded`
- `503 no_capacity` — no Firecracker hosts available and dedicated VM provisioning failed

---

### POST /v1/sandbox/{id}/exec — HandleExec

**Purpose:** Execute a shell command synchronously inside the sandbox and return output.

**Request body:**
- `command` (string, required) — shell command to execute
- `timeout_ms` (integer, optional, default 30000, max 300000)
- `env` (object, optional) — additional environment variables `{"KEY": "VALUE"}`
- `working_dir` (string, optional, default `/workspace`)

**Processing:**
1. Verify sandbox `status = 'running'` via store (cached for 1s)
2. Verify sandbox belongs to authenticated agent (from JWT sub)
3. Call `orchestrator.ExecCommand(sandboxID, command, timeoutMS, env, workingDir)`
4. Return result immediately (synchronous)

**Response 200:**
```
{
  "stdout": "string",
  "stderr": "string",
  "exit_code": 0,
  "duration_ms": 142,
  "timed_out": false
}
```

**Error cases:**
- `404 sandbox_not_found`
- `409 sandbox_not_running` — status is paused/terminated
- `403 forbidden` — sandbox belongs to a different agent
- `504 exec_timeout` — command exceeded timeout_ms

---

### POST /v1/sandbox/{id}/files — HandleWriteFile

**Purpose:** Write a file to the sandbox filesystem.

**Request body:**
- `path` (string, required) — absolute path inside sandbox (e.g., `/workspace/script.py`)
- `content` (string, required) — file content
- `encoding` (string, optional) — `utf8` (default) or `base64`
- `create_dirs` (boolean, optional, default true) — create parent directories if missing

**Response 200:**
```
{
  "path": "/workspace/script.py",
  "bytes_written": 1024,
  "created_dirs": ["/workspace"]
}
```

---

### GET /v1/sandbox/{id}/files — HandleReadFile

**Query parameters:**
- `path` (string, required) — absolute path inside sandbox

**Response 200:**
```
{
  "path": "/workspace/output.csv",
  "content": "string or base64",
  "encoding": "utf8",
  "size_bytes": 4096,
  "content_type": "text/csv"
}
```

**Error cases:**
- `404 file_not_found`
- `400 path_is_directory` — use listing endpoint instead
- `413 file_too_large` — files > 50MB must use the artifact streaming endpoint

---

### GET /v1/sandbox/{id}/status — HandleGetSandboxStatus

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "running",
  "tier": "micro",
  "runtime": "firecracker",
  "uptime_secs": 342,
  "sats_consumed": 684,
  "cpu_percent": 12.4,
  "memory_used_mb": 312,
  "memory_limit_mb": 1024,
  "host_vm": "vm-host-pool-001",
  "template": "python3",
  "created_at": "ISO8601",
  "last_billed_at": "ISO8601"
}
```

---

### POST /v1/sandbox/{id}/pause — HandlePause

**Purpose:** Snapshot VM state to GCS and stop billing.

**Request body:** (empty)

**Processing:**
1. Call `billing.StopBilling(sandboxID)` — stops ticker goroutine, settles partial interval
2. Call `orchestrator.PauseSandbox(sandboxID)` — triggers Firecracker snapshot, uploads to GCS
3. Update sandbox `status = 'paused'`, `snapshot_gcs = 'gs://...'`

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "paused",
  "snapshot_gcs": "gs://sc-snapshots/agent-id/sandbox-id/snapshot-ts.snap",
  "snapshot_size_mb": 256,
  "sats_consumed_total": 684,
  "paused_at": "ISO8601",
  "snapshot_expires_at": "ISO8601"
}
```

---

### POST /v1/sandbox/{id}/resume — HandleResume

**Processing:**
1. Verify `status = 'paused'` and snapshot exists in GCS
2. Call `orchestrator.ResumeSandbox(sandboxID)` — downloads snapshot, restores VM
3. Update sandbox `status = 'running'`
4. Call `billing.StartBilling(sandbox)` — restarts ticker

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "running",
  "wss_url": "wss://api.satoshicompute.io/v1/sandbox/{id}/shell",
  "wss_token": "string (refreshed)",
  "resumed_at": "ISO8601"
}
```

---

### DELETE /v1/sandbox/{id} — HandleTerminate

**Processing:**
1. Call `billing.StopBilling(sandboxID)` — final interval settled
2. Call `orchestrator.TerminateSandbox(sandboxID)` — destroys VM, deletes tap device, releases host memory
3. Update sandbox `status = 'terminated'`, `terminated_at = now()`
4. Delete GCS snapshot if present

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "terminated",
  "total_uptime_secs": 892,
  "total_sats_consumed": 1784,
  "terminated_at": "ISO8601"
}
```

---

### POST /v1/sandbox/{id}/delegate — HandleDelegate

**Purpose:** Return an attenuated macaroon with reduced permissions for delegation to a sub-agent.

**Request body:**
- `additional_caveats` (object) — caveats to append (e.g., `{"readonly": "true", "max_duration_secs": "300"}`)
- `expiry_secs` (integer, optional) — relative expiry from now

**Processing:**
1. Retrieve the agent's current macaroon for this sandbox from request context
2. Append the requested caveats
3. Encode the attenuated macaroon

**Response 200:**
```
{
  "delegated_token": "base64_attenuated_macaroon",
  "caveats": {"readonly": "true", "sandbox_id": "uuid", "expiry": "unix_ts"},
  "expires_at": "ISO8601"
}
```

---

### GET /v1/billing/history — HandleBillingHistory

**Query parameters:**
- `page` (integer, default 1)
- `limit` (integer, default 50, max 200)
- `sandbox_id` (string, optional) — filter to one sandbox
- `from` (ISO8601, optional)
- `to` (ISO8601, optional)

**Response 200:**
```
{
  "intervals": [
    {
      "id": "uuid",
      "sandbox_id": "uuid",
      "interval_start": "ISO8601",
      "interval_end": "ISO8601",
      "sats_charged": 20,
      "bolt12_preimage": "hex",
      "settled_at": "ISO8601"
    }
  ],
  "total_sats_all_time": 12480,
  "page": 1,
  "total_pages": 4
}
```

---

## Configuration Reference

All configuration is loaded from environment variables in Cloud Run, backed by GCP Secret Manager for sensitive values.

| Variable | Description | Source |
|---|---|---|
| `SUPABASE_URL` | Supabase project URL | Secret Manager |
| `SUPABASE_SERVICE_KEY` | Supabase service role key | Secret Manager |
| `SUPABASE_DB_HOST` | Postgres host (Supavisor transaction mode) | Secret Manager |
| `SUPABASE_DB_PASSWORD` | Postgres password | Secret Manager |
| `LND_GRPC_HOST` | LND gRPC address | Env var |
| `LND_MACAROON_HEX` | LND invoice macaroon hex | Secret Manager |
| `LND_TLS_CERT_B64` | LND TLS cert base64 | Secret Manager |
| `GCP_PROJECT_ID` | GCP project | Env var |
| `GCP_ZONE` | Primary GCP zone | Env var |
| `GCS_SNAPSHOT_BUCKET` | GCS bucket name for snapshots | Env var |
| `BILLING_INTERVAL_SECS` | Billing tick interval (default: 10) | Env var |
| `HOST_HEARTBEAT_SECS` | Host pool refresh interval (default: 30) | Env var |
| `MAX_EXEC_TIMEOUT_MS` | Maximum exec timeout (default: 300000) | Env var |
| `LOG_LEVEL` | `debug | info | warn | error` | Env var |

---

## Graceful Shutdown

On receiving SIGTERM (Cloud Run sends this 10 seconds before kill):

1. Stop accepting new HTTP connections
2. Wait for in-flight HTTP requests to complete (timeout: 8 seconds)
3. Call `billing.StopAllTickers()` — each ticker settles its current partial interval and writes a final `billing_interval` row
4. Flush any buffered log lines to Cloud Logging
5. Close Supabase pgxpool
6. Close LND gRPC connection
7. Exit 0

The 10-second Cloud Run shutdown window is tight. The billing flush must complete within 8 seconds. If any ticker cannot settle within 5 seconds, it records an `unsettled` billing interval row flagged for reconciliation.

---

## Observability

### Metrics (Prometheus, exported to Cloud Monitoring)

| Metric | Type | Labels |
|---|---|---|
| `sc_http_requests_total` | Counter | method, path, status |
| `sc_http_request_duration_ms` | Histogram | method, path |
| `sc_active_sandboxes` | Gauge | tier, runtime |
| `sc_billing_tickers_active` | Gauge | — |
| `sc_exec_duration_ms` | Histogram | tier |
| `sc_db_query_duration_ms` | Histogram | operation |

### Structured Log Fields

Every request log entry includes:
- `request_id`, `agent_id`, `sandbox_id` (where applicable)
- `method`, `path`, `status_code`, `duration_ms`
- `tier`, `runtime` (for sandbox operations)
- `error` (structured error slug, never stack trace in production)
