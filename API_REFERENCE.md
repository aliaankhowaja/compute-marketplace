# Sockt Sandbox REST API Reference

Developer-facing reference for the **12 sandbox REST endpoints** registered in [`internal/api/router.go`](internal/api/router.go). Each route invokes the same logic as the corresponding MCP tool in [`internal/mcp/tools.go`](internal/mcp/tools.go) via [`internal/api/rest_sandbox.go`](internal/api/rest_sandbox.go).

**Production base URL:** `https://api.sockt.dev`

---

## Table of contents

1. [Overview](#overview)
2. [Conventions](#conventions)
3. [Authentication](#authentication)
4. [Sandbox filesystem and paths](#sandbox-filesystem-and-paths)
5. [Async command execution](#async-command-execution)
6. [Billing models](#billing-models)
7. [Endpoint index](#endpoint-index)
8. [Endpoints (detailed)](#endpoints-detailed)
   - [GET /v1/sandboxes/tiers](#get-v1sandboxestiers)
   - [POST /v1/sandboxes](#post-v1sandboxes)
   - [GET /v1/sandboxes/{id}](#get-v1sandboxesid)
   - [POST /v1/sandboxes/{id}/exec](#post-v1sandboxesidexec)
   - [GET /v1/executions/{execution_id}](#get-v1executionsexecution_id)
   - [DELETE /v1/executions/{execution_id}](#delete-v1executionsexecution_id)
   - [POST /v1/sandboxes/{id}/files/write](#post-v1sandboxesidfileswrite)
   - [POST /v1/sandboxes/{id}/files/read](#post-v1sandboxesidfilesread)
   - [GET /v1/sandboxes/{id}/files](#get-v1sandboxesidfiles)
   - [POST /v1/sandboxes/{id}/pause](#post-v1sandboxesidpause)
   - [POST /v1/sandboxes/{id}/resume](#post-v1sandboxesidresume)
   - [DELETE /v1/sandboxes/{id}](#delete-v1sandboxesid)
9. [End-to-end workflows](#end-to-end-workflows)
10. [Common error codes](#common-error-codes)

---

## Overview

These endpoints let you provision ephemeral compute sandboxes, read/write files, run shell commands asynchronously, poll output, and manage lifecycle (pause, resume, terminate).

| REST endpoint | MCP tool | Purpose |
|---------------|----------|---------|
| `GET /v1/sandboxes/tiers` | `sandbox_list_tiers` | List tiers and pricing |
| `POST /v1/sandboxes` | `sandbox_create` | Create a sandbox |
| `GET /v1/sandboxes/{id}` | `sandbox_status` | Status, billing, metrics |
| `POST /v1/sandboxes/{id}/exec` | `sandbox_exec` | Start async command |
| `GET /v1/executions/{execution_id}` | `sandbox_exec_result` | Poll command output |
| `DELETE /v1/executions/{execution_id}` | `sandbox_exec_cancel` | Cancel running command |
| `POST /v1/sandboxes/{id}/files/write` | `sandbox_write_file` | Write a file |
| `POST /v1/sandboxes/{id}/files/read` | `sandbox_read_file` | Read a file |
| `GET /v1/sandboxes/{id}/files` | `sandbox_list_files` | List directory entries |
| `POST /v1/sandboxes/{id}/pause` | `sandbox_pause` | Pause sandbox |
| `POST /v1/sandboxes/{id}/resume` | `sandbox_resume` | Resume sandbox |
| `DELETE /v1/sandboxes/{id}` | `sandbox_terminate` | Terminate sandbox |

---

## Conventions

| Item | Value |
|------|--------|
| Request bodies | `Content-Type: application/json` where a body is used |
| Successful JSON responses | `Content-Type: application/json` |
| Correlation | Every response includes `X-Request-ID` |
| Path IDs | `sandbox_id` is a UUID in `{id}`; `execution_id` is an opaque string like `exec_abc123` |
| Timestamps | RFC3339 UTC (e.g. `2026-05-28T11:54:13Z`) |

### Error envelope

Non-success responses use:

```json
{
  "code": "forbidden",
  "error": "forbidden",
  "message": "human-readable explanation",
  "hint": "optional remediation hint"
}
```

Rate limiting returns **429** with plain text body `rate limited` (not JSON).

---

## Authentication

All 12 routes share [`mcp.AuthMiddleware`](internal/mcp/auth.go) (same as `POST /v1/mcp`).

### Credential types

| Credential | Header | Used for |
|------------|--------|----------|
| **API key** | `Authorization: Bearer sockt_live_...` | Credits sandboxes; ties requests to your user account |
| **Sandbox token** | `Authorization: Bearer sbx_...` | Lightning sandboxes; scoped to one sandbox |
| **Sandbox token (alt)** | Query `?sandbox_token=sbx_...` or JSON field `sandbox_token` | Same as bearer `sbx_...` where supported |
| **None** | Omit `Authorization` | Allowed for tier listing and lightning `POST /v1/sandboxes` only |

JWT dashboard tokens are **not** accepted on these routes.

### Per-endpoint auth requirements

| Endpoint | API key | Sandbox token | Anonymous |
|----------|---------|---------------|-----------|
| `GET /v1/sandboxes/tiers` | Optional | Optional | Yes |
| `POST /v1/sandboxes` (lightning) | Optional | — | Yes |
| `POST /v1/sandboxes` (credits) | **Required** | — | No |
| All `{id}` sandbox routes | Yes | Yes | No |
| Exec / execution routes | Yes | Yes | No |

### Access control

After authentication, sandbox operations call `principalCanAccess`:

- **API key:** sandbox `UserID` must match the key’s user.
- **Sandbox token:** token must match the sandbox row’s stored `SandboxToken`.
- **Mismatch:** **403** `forbidden`.

### Exec ownership (REST)

`POST .../exec`, `GET/DELETE /v1/executions/{execution_id}` require a **caller key** derived from:

- API key principal → `apikey:{api_key_id}`
- Sandbox token (header, query, or body) → `token:{sandbox_token}`

You may only poll or cancel executions started with the **same** credential that started them. Another key/token gets **403** `forbidden` (“execution belongs to another caller”).

### Rate limit

Shared with MCP: default **120 requests/minute** per principal (or per IP when anonymous). Excess → **429** `rate limited`.

---

## Sandbox filesystem and paths

Default sandbox home (`SANDBOX_ROOT`) is `/home/sandbox`. New sandboxes start with an **empty** filesystem.

### Path rules (files + `working_dir`)

Paths are **logical** paths under `SANDBOX_ROOT`:

| Path you send | Resolves to |
|---------------|-------------|
| `.` or omitted `working_dir` | Sandbox home |
| `/` | Sandbox home |
| `myproject` | `/home/sandbox/myproject` |
| Absolute under root (e.g. `/home/sandbox/foo`) | Allowed only if equal to or under `SANDBOX_ROOT` |

Use `GET /v1/sandboxes/{id}/files` before assuming files exist.

---

## Async command execution

1. `POST /v1/sandboxes/{id}/exec` → returns immediately with `execution_id`, `status: "running"`. **No stdout/stderr in this response.**
2. Poll `GET /v1/executions/{execution_id}` until `status` is `completed`, `failed`, or `cancelled`.
3. Each poll returns **new** output chunks since the previous poll (`output[]` with `stream` + `chunk`).
4. Optional: `DELETE /v1/executions/{execution_id}` to cancel.
5. Up to **10 concurrent** executions per caller key (configurable server-side).
6. Completed executions are retained only for a **limited time** — poll promptly.

Commands run as: `/bin/bash -lc "<your command>"`.

---

## Billing models

### Credits (`billing_method: "credits"`)

- Requires `Authorization: Bearer sockt_live_...`.
- Usage deducts **USD credit balance** automatically (no BOLT11 invoices).
- `POST /v1/sandboxes` provisions after allocating credits (typically ~1 hour minimum per tier).
- If balance runs out, sandbox may pause; top up credits externally, then `POST .../resume`.
- `DELETE /v1/sandboxes/{id}` may return `refund` with unused allocated cents credited back.

### Lightning (`billing_method: "lightning"`)

- No API key required to create.
- `POST /v1/sandboxes` returns `invoice` (BOLT11) + `sandbox_token` immediately.
- Pay invoice → status moves through `awaiting_payment` → `pending` (provisioning) → `running`.
- Balance decreases every second while running (`msats_per_second` for the tier).
- Poll `GET /v1/sandboxes/{id}` using `next_poll_secs` from each response (adaptive **5–60 s**).
- When `pending_invoice` appears, pay the BOLT11 immediately or the sandbox auto-pauses.
- `warning_level` values: `provisioning`, `ok`, `low_balance`, `awaiting_payment`, `out_of_balance`.
- Optional `lightning_address` on terminate for async refund of unused prepaid balance.

---

## Endpoint index

| # | Method | Path |
|---|--------|------|
| 1 | GET | `/v1/sandboxes/tiers` |
| 2 | POST | `/v1/sandboxes` |
| 3 | GET | `/v1/sandboxes/{id}` |
| 4 | POST | `/v1/sandboxes/{id}/exec` |
| 5 | GET | `/v1/executions/{execution_id}` |
| 6 | DELETE | `/v1/executions/{execution_id}` |
| 7 | POST | `/v1/sandboxes/{id}/files/write` |
| 8 | POST | `/v1/sandboxes/{id}/files/read` |
| 9 | GET | `/v1/sandboxes/{id}/files` |
| 10 | POST | `/v1/sandboxes/{id}/pause` |
| 11 | POST | `/v1/sandboxes/{id}/resume` |
| 12 | DELETE | `/v1/sandboxes/{id}` |

---

## Endpoints (detailed)

---

### GET `/v1/sandboxes/tiers`

**MCP tool:** `sandbox_list_tiers`

**Purpose:** List every sandbox tier with Lightning rates (msats/sats per second), credits rates (USD cents per second), and example costs for 1 minute, 1 hour, and 1 day. Call this **before** `POST /v1/sandboxes` to choose tier and estimate cost.

#### Authentication

| Required | Notes |
|----------|-------|
| No | Public catalog |

#### Parameters

| Location | Name | Type | Required | Description |
|----------|------|------|----------|-------------|
| — | — | — | — | No path, query, or body parameters |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `note` | string | Explains which rate fields apply to lightning vs credits billing |
| `tiers` | array | One object per tier (see below) |

**Each element of `tiers`:**

| Field | Type | Description |
|-------|------|-------------|
| `tier` | string | Tier id: `nano`, `micro`, `standard`, `gpu_small`, `gpu_large` |
| `msats_per_second` | integer | Lightning billing rate (millisatoshis per second of runtime) |
| `sats_per_second` | number | `msats_per_second / 1000` |
| `usd_cents_per_second` | number | Credits billing rate in USD cents per second |
| `cost_per_minute_sats` | number | Example: cost to run 60 seconds at this tier (lightning) |
| `cost_per_hour_sats` | number | Example: 1 hour (lightning) |
| `cost_per_day_sats` | number | Example: 24 hours (lightning) |
| `cost_per_hour_usd` | number | Example: 1 hour (credits), in USD |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 500 | `store_error` | Database unavailable |

#### Example

```bash
curl -sS "https://api.sockt.dev/v1/sandboxes/tiers"
```

---

### POST `/v1/sandboxes`

**MCP tool:** `sandbox_create`

**Purpose:** Provision a new sandbox pod. **Credits:** charges your account and starts provisioning (30–90 s). **Lightning:** returns a BOLT11 prepay invoice and `sandbox_token`; pod boots after payment confirms.

#### Authentication

| Billing | Required |
|---------|----------|
| `credits` | `Authorization: Bearer sockt_live_...` |
| `lightning` | Optional (anonymous allowed) |

#### Parameters

**JSON body:**

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `tier` | string | **Yes** | — | Hardware tier: `nano`, `micro`, `standard`, `gpu_small`, `gpu_large`. Must exist in pricing table. |
| `billing_method` | string | No | `lightning` if no API key; `credits` if API key present | `lightning` or `credits`. Invalid value → **400** `invalid_params`. |
| `label` | string | No | — | Optional human label stored on the sandbox (for your records). |
| `prepaid_sats` | integer | No (lightning) | Server default (~30 s of compute at tier rate) | Lightning only. Initial prepay amount in **satoshis** (minimum 1). Server mints BOLT11 for this amount. Higher value → longer initial runtime before first top-up. |
| `initial_credits_cents` | integer | No (credits) | ~1 hour at tier rate | Credits only. USD **cents** to allocate to this sandbox up front. If omitted or too low, server uses at least 1 hour of compute at tier rate. Below minimum → **400** `invalid_params`. |

#### Success response

| Billing | HTTP | Body |
|---------|------|------|
| Lightning | **201 Created** | Invoice + token object (below) |
| Credits | **201 Created** | Full sandbox database row (Go struct field names) |

**Lightning — typical fields:**

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | `awaiting_payment` until invoice is paid |
| `sandbox_id` | string | UUID — use in all `{id}` paths |
| `sandbox_token` | string | `sbx_...` — use as Bearer or `sandbox_token` query/body on later calls |
| `billing_method` | string | `lightning` |
| `tier` | string | Tier you requested |
| `runtime` | string | `cpu` or `gpu` (public label) |
| `invoice` | string | BOLT11 payment request — pay from any Lightning wallet |
| `payment_hash` | string | Hex payment hash for the invoice |
| `amount_msats` | integer | Invoice amount in millisatoshis |
| `amount_sats` | integer | Invoice amount in satoshis |
| `msats_per_second` | integer | Ongoing burn rate after prepay is consumed |
| `prepaid_seconds` | integer | Approximate seconds of compute covered by initial prepay |
| `expires_at` | string | RFC3339 — invoice expiry |
| `next_step` | string | Human hint: pay invoice, then poll status |
| `auth_hint` | string | How to pass `sandbox_token` on subsequent requests |

**Credits — typical fields** (persisted model; keys match Go exports):

| Field | Type | Description |
|-------|------|-------------|
| `ID` | string | Sandbox UUID |
| `Status` | string | Often `pending` then `running` after orchestration |
| `BillingMethod` | string | `credits` |
| `Tier` | string | Requested tier |
| `Template` | string | Internal template name (e.g. `sockt-micro`) |
| `Runtime` | string | Orchestrator runtime id (e.g. `runpod_cpu`) |
| `PodID` | string | Provider pod id when provisioned |
| `PodProxyURL` | string | Data-plane base URL for exec/files |
| `PrepaidBalanceSubcents` | integer | Allocated credit subcents for this sandbox |
| `UserID` | string | Owner user UUID |
| `APIKeyID` | string | API key used to create |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 400 | `invalid_params` | Missing `tier`, bad `billing_method`, JSON parse error |
| 400 | `bad_tier` | Unknown tier |
| 401 | `unauthorized` | Credits without API key |
| 402 | `insufficient_credits` | Not enough account balance |
| 502 | `provisioning_error` | Orchestrator failed after credit allocation |
| 502 | `invoice_failed` | Lightning invoice could not be created |
| 503 | `orchestrator_disabled` | Tier mapping / RunPod not configured |
| 503 | `payment_disabled` | Lightning not configured on server |

#### Examples

**Lightning (no API key):**

```bash
curl -sS -X POST "https://api.sockt.dev/v1/sandboxes" \
  -H "Content-Type: application/json" \
  -d '{"billing_method":"lightning","tier":"micro","label":"my-run"}'
```

**Credits:**

```bash
curl -sS -X POST "https://api.sockt.dev/v1/sandboxes" \
  -H "Authorization: Bearer sockt_live_YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"billing_method":"credits","tier":"micro","label":"my-run"}'
```

---

### GET `/v1/sandboxes/{id}`

**MCP tool:** `sandbox_status`

**Purpose:** Read sandbox lifecycle status, billing state, remaining balance, warning level, and live runtime metrics. For Lightning, this is the primary polling endpoint during provisioning and while running.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key **or** sandbox token |

#### Parameters

| Location | Name | Type | Required | Description |
|----------|------|------|----------|-------------|
| Path | `id` | UUID | **Yes** | Sandbox id from create response |
| Query | `sandbox_token` | string | No* | `sbx_...` if not using `Authorization: Bearer sbx_...` |

\*Required for lightning callers without an API key.

#### Success response — **200 OK**

**Core fields (all billing methods):**

| Field | Type | Description |
|-------|------|-------------|
| `sandbox_id` | string | Sandbox UUID |
| `status` | string | e.g. `awaiting_payment`, `pending`, `running`, `paused`, `terminated` |
| `tier` | string | Tier name |
| `template` | string | Template label (e.g. `sockt-micro`) |
| `runtime` | string | `cpu` or `gpu` |
| `billing_method` | string | `lightning` or `credits` |
| `consumed_msats` | integer | Total consumed (see billing note below) |
| `metrics_stale` | boolean | `true` if live runtime metrics could not be fetched |
| `msats_per_second` | integer | Tier lightning rate (if configured) |
| `sats_per_second` | number | Derived from `msats_per_second` |

**Lightning-only fields:**

| Field | Type | Description |
|-------|------|-------------|
| `sandbox_token` | string | Echo of token (for client convenience) |
| `prepaid_balance_msats` | integer | Remaining prepaid millisatoshis |
| `seconds_remaining` | integer | Approx `prepaid_balance_msats / msats_per_second` |
| `warning_level` | string | See table below |
| `next_poll_secs` | integer | **Wait this many seconds** before calling this endpoint again |
| `pending_invoice` | object | Present when payment is required (see below) |

**`warning_level` values:**

| Value | Meaning | Client action |
|-------|---------|---------------|
| `provisioning` | Paid, pod booting (`status` often `pending`) | Poll every **5 s** until `status` is `running` |
| `ok` | Healthy balance | Poll again in `next_poll_secs` |
| `low_balance` | Running low | Expect `pending_invoice` soon; poll in `next_poll_secs` |
| `awaiting_payment` | Invoice unpaid | Pay `pending_invoice.bolt11` immediately; poll every `next_poll_secs` (~5 s) |
| `out_of_balance` | Paused, no balance | Pay invoice, then `POST .../resume` |

**`pending_invoice` object:**

| Field | Type | Description |
|-------|------|-------------|
| `bolt11` | string | BOLT11 to pay |
| `payment_hash` | string | Hex payment hash |
| `amount_msats` | integer | Invoice amount |
| `expires_at` | string | RFC3339 expiry |

**Live runtime fields** (when pod is reachable, `metrics_stale: false`):

| Field | Type | Description |
|-------|------|-------------|
| `balance_local` | varies | Runtime-reported balance snapshot |
| `runtime_*` | varies | Prefixed metrics from pod `/status` (e.g. `runtime_balance`, `runtime_consumed_total`, `runtime_rate_per_second`, `runtime_billing_method`, `runtime_topup_in_flight`) |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 400 | `invalid_params` | Invalid `id` UUID |
| 403 | `forbidden` | Token/key cannot access this sandbox |
| 404 | `not_found` | Unknown sandbox id |

#### Example

```bash
SID="YOUR_SANDBOX_UUID"
TOK="sbx_YOUR_TOKEN"
curl -sS "https://api.sockt.dev/v1/sandboxes/${SID}?sandbox_token=${TOK}"
```

---

### POST `/v1/sandboxes/{id}/exec`

**MCP tool:** `sandbox_exec`

**Purpose:** Start a shell command **asynchronously** in the sandbox. Returns `execution_id` immediately; retrieve output via `GET /v1/executions/{execution_id}`.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token (header, query, or body) |

#### Parameters

| Location | Name | Type | Required | Default | Description |
|----------|------|------|----------|---------|-------------|
| Path | `id` | UUID | **Yes** | — | Sandbox id |
| Query | `sandbox_token` | string | No | — | Alternative to Bearer `sbx_...` |
| Body | `command` | string | **Yes** | — | Shell command executed as `/bin/bash -lc "<command>"` |
| Body | `working_dir` | string | No | `.` (sandbox home) | Logical working directory under `SANDBOX_ROOT` |
| Body | `timeout_ms` | integer | No | `30000` | Client-side scheduling hint. `0` = unlimited (long jobs). Server caps non-zero values at **300000** (5 min) for sync fallback paths; async stream uses runtime “no timeout” with a server max duration (hours). |
| Body | `sandbox_token` | string | No | — | Alternative to Bearer/query |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `execution_id` | string | Opaque id for polling (e.g. `exec_...`) |
| `status` | string | Always `running` on success |
| `sandbox_id` | string | Sandbox UUID |
| `command` | string | Echo of command string |
| `next_step` | string | Instructs client to poll exec result endpoint |

**No `stdout` / `stderr` in this response.**

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 400 | `invalid_params` | Missing `command` or bad `id` |
| 401 | `unauthorized` | No caller key (missing API key and sandbox token) |
| 403 | `forbidden` | No access to sandbox |
| 404 | `not_found` | Unknown sandbox |
| 429 | `too_many_executions` | More than 10 concurrent executions for this caller |
| 400 | `pod_unreachable` | Sandbox has no data-plane URL yet |

#### Example

```bash
curl -sS -X POST "https://api.sockt.dev/v1/sandboxes/${SID}/exec?sandbox_token=${TOK}" \
  -H "Content-Type: application/json" \
  -d '{"command":"python3 main.py","working_dir":".","timeout_ms":120000}'
```

---

### GET `/v1/executions/{execution_id}`

**MCP tool:** `sandbox_exec_result`

**Purpose:** Poll buffered **stdout/stderr** chunks and execution status. Call repeatedly until status is terminal. Each call returns only chunks **since the last poll**.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | Same credential that started the execution |

#### Parameters

| Location | Name | Type | Required | Description |
|----------|------|------|----------|-------------|
| Path | `execution_id` | string | **Yes** | From `POST .../exec` response |
| Query | `sandbox_token` | string | No | If using sandbox token auth |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `execution_id` | string | Echo of requested id |
| `status` | string | `running`, `completed`, `failed`, or `cancelled` |
| `output` | array | New chunks since last poll (may be empty while running) |
| `exit_code` | integer | Present when finished (shell exit code) |
| `error` | string | Present on failure (error message) |
| `pending_invoice` | object | Lightning only: same shape as status endpoint when top-up needed |

**Each `output[]` element:**

| Field | Type | Description |
|-------|------|-------------|
| `stream` | string | `stdout` or `stderr` |
| `chunk` | string | Raw text fragment (may be partial lines) |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 401 | `unauthorized` | No caller key |
| 403 | `forbidden` | Execution started by a different caller |
| 404 | `not_found` | Unknown or expired execution id |

#### Example

```bash
EID="exec_..."
curl -sS "https://api.sockt.dev/v1/executions/${EID}?sandbox_token=${TOK}"
```

---

### DELETE `/v1/executions/{execution_id}`

**MCP tool:** `sandbox_exec_cancel`

**Purpose:** Request cancellation of a **running** execution. Returns immediately with `status: "cancelled"`; poll result endpoint to confirm.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | Same credential that started the execution |

#### Parameters

| Location | Name | Type | Required | Description |
|----------|------|------|----------|-------------|
| Path | `execution_id` | string | **Yes** | Execution to cancel |
| Query | `sandbox_token` | string | No | Sandbox token auth |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `execution_id` | string | Cancelled execution id |
| `status` | string | `cancelled` |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 401 | `unauthorized` | No caller key |
| 403 | `forbidden` | Wrong caller |
| 404 | `not_found` | Already completed or unknown id |

#### Example

```bash
curl -sS -X DELETE "https://api.sockt.dev/v1/executions/${EID}?sandbox_token=${TOK}"
```

---

### POST `/v1/sandboxes/{id}/files/write`

**MCP tool:** `sandbox_write_file`

**Purpose:** Create or overwrite a file in the sandbox. Content is sent as UTF-8 text or base64; the server encodes to base64 on the wire to the runtime.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token |

#### Parameters

| Location | Name | Type | Required | Default | Description |
|----------|------|------|----------|---------|-------------|
| Path | `id` | UUID | **Yes** | — | Sandbox id |
| Query | `sandbox_token` | string | No | — | Token auth alternative |
| Body | `path` | string | **Yes** | — | Destination file path (logical, under `SANDBOX_ROOT`) |
| Body | `content` | string | **Yes** | — | File contents (interpreted per `encoding`) |
| Body | `encoding` | string | No | `utf8` | `utf8`: content is plain text. `base64`: content is standard base64 of raw bytes. |
| Body | `create_dirs` | boolean | No | `false` | If `true`, create parent directories as needed |
| Body | `sandbox_token` | string | No | — | Token auth alternative |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `path` | string | Path written |
| `bytes_written` | integer | Approximate encoded size written |
| `created_dirs` | boolean | Whether parent dirs were created |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 400 | `invalid_params` | Missing `path` or `content` |
| 400 | `pod_unreachable` | No data plane |
| 403 | `forbidden` | No access |
| 502 | `host_error` | Runtime rejected write |

#### Example

```bash
curl -sS -X POST "https://api.sockt.dev/v1/sandboxes/${SID}/files/write?sandbox_token=${TOK}" \
  -H "Content-Type: application/json" \
  -d '{"path":"main.py","content":"print(42)\n","encoding":"utf8","create_dirs":true}'
```

---

### POST `/v1/sandboxes/{id}/files/read`

**MCP tool:** `sandbox_read_file`

**Purpose:** Read a file from the sandbox filesystem.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token |

#### Parameters

| Location | Name | Type | Required | Default | Description |
|----------|------|------|----------|---------|-------------|
| Path | `id` | UUID | **Yes** | — | Sandbox id |
| Query | `sandbox_token` | string | No | — | Token auth |
| Body | `path` | string | **Yes** | — | File path to read |
| Body | `encoding` | string | No | `utf8` | `utf8`: response in `content`. `base64`: response in `content_b64`. |
| Body | `max_bytes` | integer | No | unlimited | Truncate read to at most this many bytes; sets `truncated: true` if capped |
| Body | `sandbox_token` | string | No | — | Token auth |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `path` | string | Path read |
| `size_bytes` | integer | Decoded file size |
| `encoding` | string | Encoding you requested |
| `truncated` | boolean | `true` if `max_bytes` cut the file |
| `content` | string | File text (when `encoding=utf8`) |
| `content_b64` | string | Base64 file bytes (when `encoding=base64`) |

#### Errors

Same family as write: `invalid_params`, `pod_unreachable`, `forbidden`, `host_error`.

#### Example

```bash
curl -sS -X POST "https://api.sockt.dev/v1/sandboxes/${SID}/files/read?sandbox_token=${TOK}" \
  -H "Content-Type: application/json" \
  -d '{"path":"main.py","encoding":"utf8"}'
```

---

### GET `/v1/sandboxes/{id}/files`

**MCP tool:** `sandbox_list_files`

**Purpose:** List files and directories in a sandbox directory (single level from runtime).

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token |

#### Parameters

| Location | Name | Type | Required | Default | Description |
|----------|------|------|----------|---------|-------------|
| Path | `id` | UUID | **Yes** | — | Sandbox id |
| Query | `path` | string | No | `/` | Directory to list (logical path) |
| Query | `recursive` | string | No | `false` | Accepted by REST (`true` enables flag). Forwarded to tool layer; listing is currently **single-directory** from runtime. |
| Query | `max_depth` | integer | No | — | Accepted when `recursive=true`; depth limit for future recursive listing. |
| Query | `sandbox_token` | string | No | — | Token auth |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `entries` | array | Directory entries |
| `total_entries` | integer | Count of entries |

**Each `entries[]` element:**

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | File or directory name |
| `size` | integer | Size in bytes (files) |
| `mod_ts` | integer | Modification time (Unix seconds) |
| `is_dir` | boolean | `true` if directory |
| `is_file` | boolean | `true` if regular file |

#### Example

```bash
curl -sS "https://api.sockt.dev/v1/sandboxes/${SID}/files?path=.&sandbox_token=${TOK}"
```

---

### POST `/v1/sandboxes/{id}/pause`

**MCP tool:** `sandbox_pause`

**Purpose:** Pause a running sandbox workload at the orchestrator (stops compute billing meter on provider side where supported).

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token |

#### Parameters

| Location | Name | Type | Required | Description |
|----------|------|------|----------|-------------|
| Path | `id` | UUID | **Yes** | Sandbox id |
| Query | `sandbox_token` | string | No | Token auth |
| Body | `sandbox_token` | string | No | Optional JSON body with token only |

Empty body is valid.

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `sandbox_id` | string | Sandbox UUID |
| `status` | string | `paused` |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 502 | `pause_failed` | Orchestrator error |
| 403 | `forbidden` | No access |

---

### POST `/v1/sandboxes/{id}/resume`

**MCP tool:** `sandbox_resume`

**Purpose:** Resume a paused sandbox. Use after lightning top-up or credits account refill when sandbox was paused for balance.

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token |

#### Parameters

Same as pause (path `id`, optional `sandbox_token` in query or body).

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `sandbox_id` | string | Sandbox UUID |
| `status` | string | `running` |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 502 | `resume_failed` | Orchestrator error |

---

### DELETE `/v1/sandboxes/{id}`

**MCP tool:** `sandbox_terminate`

**Purpose:** Permanently delete the sandbox and underlying pod. Stops all billing. For lightning, optionally refund unused prepaid balance to a Lightning address.

**REST note:** The handler always sets `confirm: true` internally — you do **not** need to send `confirm` in the body (unlike raw MCP).

#### Authentication

| Required | Notes |
|----------|-------|
| Yes | API key or sandbox token |

#### Parameters

| Location | Name | Type | Required | Description |
|----------|------|------|----------|-------------|
| Path | `id` | UUID | **Yes** | Sandbox to destroy |
| Query | `sandbox_token` | string | No | Token auth |
| Body | `sandbox_token` | string | No | Token auth |
| Body | `lightning_address` | string | No | Lightning only: `user@domain` address for **async refund** of remaining prepaid msats (converted to sats, rounded up) |

#### Success response — **200 OK**

| Field | Type | Description |
|-------|------|-------------|
| `sandbox_id` | string | Terminated sandbox UUID |
| `status` | string | `terminated` |
| `consumed_msats_total` | integer | Total consumed for lifetime of sandbox |
| `terminated_at` | string | RFC3339 termination time |
| `refund` | object | Optional — see below |

**`refund` object (credits):**

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | `credited` |
| `amount_cents` | integer | USD cents returned to your account |

**`refund` object (lightning, with `lightning_address`):**

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | `sent`, `failed`, or `skipped` |
| `amount_sats` | integer | Refund amount attempted |
| `address` | string | Destination address |
| `reason` | string | Present when `skipped` or `failed` |

#### Errors

| HTTP | `code` | When |
|------|--------|------|
| 502 | `terminate_failed` | Orchestrator could not delete pod |

#### Example

```bash
# Lightning with refund address
curl -sS -X DELETE "https://api.sockt.dev/v1/sandboxes/${SID}?sandbox_token=${TOK}" \
  -H "Content-Type: application/json" \
  -d '{"lightning_address":"you@wallet.example"}'

# Credits
curl -sS -X DELETE "https://api.sockt.dev/v1/sandboxes/${SID}" \
  -H "Authorization: Bearer sockt_live_YOUR_KEY"
```

---

## End-to-end workflows

### Credits: create, upload, run, terminate

```bash
API="https://api.sockt.dev"
KEY="Bearer sockt_live_YOUR_KEY"

# 1. Create (wait until Status is running — may take 30–90s)
CREATE=$(curl -sS -X POST "$API/v1/sandboxes" \
  -H "Authorization: $KEY" -H "Content-Type: application/json" \
  -d '{"billing_method":"credits","tier":"micro"}')
SID=$(echo "$CREATE" | jq -r '.ID')

# 2. Write file
curl -sS -X POST "$API/v1/sandboxes/$SID/files/write" \
  -H "Authorization: $KEY" -H "Content-Type: application/json" \
  -d '{"path":"main.py","content":"import random\nfor i in range(10):\n    print(random.randint(1,100))\n","encoding":"utf8","create_dirs":true}'

# 3. Exec
EXEC=$(curl -sS -X POST "$API/v1/sandboxes/$SID/exec" \
  -H "Authorization: $KEY" -H "Content-Type: application/json" \
  -d '{"command":"python3 main.py","timeout_ms":120000}')
EID=$(echo "$EXEC" | jq -r '.execution_id')

# 4. Poll until completed
until [ "$(curl -sS "$API/v1/executions/$EID" -H "Authorization: $KEY" | jq -r '.status')" != "running" ]; do sleep 1; done
curl -sS "$API/v1/executions/$EID" -H "Authorization: $KEY" | jq .

# 5. Terminate
curl -sS -X DELETE "$API/v1/sandboxes/$SID" -H "Authorization: $KEY"
```

### Lightning: create, pay, run, terminate

```bash
API="https://api.sockt.dev"

# 1. Create (no API key)
CREATE=$(curl -sS -X POST "$API/v1/sandboxes" \
  -H "Content-Type: application/json" \
  -d '{"billing_method":"lightning","tier":"micro"}')
SID=$(echo "$CREATE" | jq -r '.sandbox_id')
TOK=$(echo "$CREATE" | jq -r '.sandbox_token')
INVOICE=$(echo "$CREATE" | jq -r '.invoice')
# Pay INVOICE with your Lightning wallet, then:

# 2. Poll status until running
until [ "$(curl -sS "$API/v1/sandboxes/$SID?sandbox_token=$TOK" | jq -r '.status')" = "running" ]; do sleep 5; done

# 3. Write + exec + poll (same as credits, but use ?sandbox_token= on each call)
# 4. DELETE when done
curl -sS -X DELETE "$API/v1/sandboxes/$SID?sandbox_token=$TOK"
```

---

## Common error codes

| `code` | Typical HTTP | Meaning |
|--------|--------------|---------|
| `invalid_params` | 400 | Missing/invalid argument |
| `bad_request` | 400 | Malformed JSON body |
| `unauthorized` | 401 | Missing or invalid credentials |
| `forbidden` | 403 | Valid auth but wrong sandbox or execution owner |
| `not_found` | 404 | Sandbox or execution not found |
| `insufficient_credits` | 402 | Account balance too low |
| `too_many_executions` | 429 | Concurrent exec limit (10 per caller) |
| `pod_unreachable` | 400 | Sandbox not ready (no pod URL) |
| `pause_failed` / `resume_failed` / `terminate_failed` | 502 | Orchestrator error |
| `host_unreachable` / `host_error` | 502/503 | Data-plane runtime error |
| `store_error` | 500 | Database error |
| `orchestrator_disabled` | 503 | Provisioning not configured |
| `payment_disabled` | 503 | Lightning payments unavailable |
| `invoice_failed` | 502 | Could not create BOLT11 |

---

## Source of truth

- Routes: [`internal/api/router.go`](internal/api/router.go)
- REST adapters: [`internal/api/rest_sandbox.go`](internal/api/rest_sandbox.go)
- Tool schemas and descriptions: [`internal/mcp/tools.go`](internal/mcp/tools.go)
- Business logic: [`internal/mcp/service.go`](internal/mcp/service.go), [`internal/mcp/proxy.go`](internal/mcp/proxy.go)
