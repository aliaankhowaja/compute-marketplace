# Sockt Control Plane HTTP API Reference

This document describes the HTTP surface exposed by the **sockt control plane** (`sockt-cp`, [`cmd/control-plane`](cmd/control-plane/main.go)). It is maintained against the routes registered in [`internal/api/router.go`](internal/api/router.go).

**Scope:** Control-plane lifecycle, user/wallet/dashboard, MCP JSON-RPC over HTTP, runtime callbacks, and Stripe webhooks. Sandbox **data-plane** URLs (exec, files, shell) are not documented here unless they appear as fields on stored sandbox rows.

---

## Conventions

- **Base URL:** Deployment-specific (for example Cloud Run). Paths below are absolute from `/`.
- **Content-Type:** Use `application/json` for bodies where a JSON body is specified. Successful JSON responses use `application/json`.
- **`X-Request-ID`:** Responses include this header. If the client omitted it, the server generates one ([`recoverReqIDLog`](internal/api/respond.go)).
- **Structured errors (REST):** Most handlers use [`writeErr`](internal/api/respond.go):

  ```json
  { "code": "...", "error": "...", "message": "..." }
  ```

  alongside an appropriate HTTP status code.

- **Plain-text errors:** [`JWTAuth`](internal/auth/jwt.go) rejects missing/invalid JWTs with [`http.Error`](https://pkg.go.dev/net/http#Error) bodies (plain text such as `"missing authorization"`, `"invalid token"`), not JSON. Plan clients accordingly.

- **Rate limiting:** JWT-protected routes, MCP POST, and the outer wrapper around sandbox/API-key+L402 routing use [`auth.NewRateLimiter(300)`](internal/api/router.go) — approximately **300 requests per minute per limiter key**, with burst derived from RPM ([`internal/auth/ratelimit.go`](internal/auth/ratelimit.go)). Excess requests receive **429** with body `rate limited` (plain text). The limit key is [`Principal.BalanceKey`](internal/auth/context.go); where the principal is not set yet inside the sandbox route wrapper, requests share the **`anon`** key.

---

## JSON encoding of persisted models

Types from [`internal/store/store.go`](internal/store/store.go) (`User`, `Sandbox`, `Agent`, `TierPricing`, `BillingInterval`, `CreditTxSummary`, etc.) are serialized with **`encoding/json` defaults**: **no struct tags**, so JSON object keys match **Go exported field names** (for example `ID`, `UserID`, `CreditBalanceUSDCents`, `PodProxyURL`). Do not assume `snake_case` for these payloads.

---

## Authentication overview

```mermaid
flowchart LR
  subgraph sandbox_routes [/v1/sandbox paths]
    A["Authorization Bearer"]
    A --> B{"prefix sockt_live_?"}
    B -->|"yes"| C[API key principal]
    B -->|"no"| D[L402 macaroon + preimage]
  end
```

| Mechanism | When | Headers / behavior |
|-----------|------|---------------------|
| **None** | Public catalog and health | No auth. |
| **User JWT** | `/v1/user/*`, `/v1/wallet/*` | `Authorization: Bearer <jwt>`. HS256, `sub` claim must be user UUID ([`JWTAuth`](internal/auth/jwt.go)). |
| **API key** | Sandbox routes when bearer token starts with `sockt_live_` | Same `Bearer` header; validated via API key middleware ([`internal/auth/apikey.go`](internal/auth/apikey.go)). Credits sandboxes require this path. |
| **L402** | Sandbox routes when Bearer is **not** an API key | `Authorization: L402 <base64_macaroon>:<preimage_hex>` ([`parseL402Auth`](internal/auth/l402.go)). First request without credentials: **`X-Agent-Id`** header (UUID) + missing/invalid Authorization → **402** JSON body: `macaroon`, `invoice`, `payment_hash`, `expires_at`. Lightning billing sandboxes require L402 principal. |
| **MCP composite** | `POST /v1/mcp` | Order: API key → L402 → user JWT ([`AuthMiddleware`](internal/mcp/auth.go)). |
| **Callback secret** | Runtime POST callbacks | `Authorization: Bearer <CALLBACK_SECRET>` ([`BearerSecret`](internal/auth/jwt.go)). |
| **Stripe signing** | `POST /internal/webhooks/stripe` | **`Stripe-Signature`** on raw webhook body; verifies HMAC ([`verifyStripePayload`](internal/api/stripe.go)). |

### Sandbox ownership

After authentication, sandbox access checks [`canAccessSandbox`](internal/api/handlers.go): **credits/API-key** callers must match `sandbox.UserID`; **Lightning/L402** callers must match `sandbox.AgentID`. Mismatches yield **403** `forbidden` on affected routes.

---

## Endpoint catalog

### `GET /healthz`

**Auth:** None  
**Purpose:** Liveness / dependency sniff.

**Response 200:**

```json
{
  "ok": true,
  "lnd_connected": false,
  "supabase_ok": false,
  "runpod_configured": false
}
```

(Field semantics: non-empty transactional DSN, LND client present, `RunPodAPIKey` configured — see router implementation.)

---

### `GET /v1/pricing`

**Auth:** None  
**Purpose:** Tier pricing rows from the database.

**Response 200:** JSON array of [`TierPricing`](internal/store/store.go) (`Tier`, `MsatsPerSecond`, `USDCentsPerSecondX100`).  
**Errors:** `store_error` (500).

---

### `GET /v1/mcp/health`

**Auth:** None  
**Purpose:** MCP transport capability probe.

**Response 200:** From [`HealthPayload`](internal/mcp/mcp.go):

- `status`, `protocol_version`, `transport`, `json_rpc_per_post`.

---

### `POST /v1/mcp`

**Auth:** API key (`sockt_live_…`) OR L402 OR Supabase JWT (see [`AuthMiddleware`](internal/mcp/auth.go)). Wrapped with rate limiting.

**Purpose:** Single JSON-RPC 2.0 request per POST — “Streamable HTTP MVP” ([`internal/mcp/handler.go`](internal/mcp/handler.go)). Request body capped by config (`MCPRequestBodyLimit`, default 4 MiB).

**Supported methods:**

| Method | Notes |
|--------|-------|
| `initialize` | Response sets header **`Mcp-Session-Id`** to a new session UUID. Result includes `protocolVersion`, `capabilities`, `serverInfo`, `session`. |
| `tools/list` | Returns MCP tool descriptors from [`toolList()`](internal/mcp/tools.go) (full `inputSchema` per tool). |
| `tools/call` | Params: `{ "name": "<tool>", "arguments": { … } }`. Errors return HTTP 200 with JSON-RPC error object (`code`, `message`, optional `data` with `error_slug`). |
| `ping` | Empty JSON object result. |

**Errors:** Always HTTP **200** for parseable JSON-RPC wrappers; RPC errors appear in `.error`. Parse failures use JSON-RPC `-32700` / `-32600` style codes ([`internal/mcp/mcp.go`](internal/mcp/mcp.go)).

**Tool names:** `sandbox_create`, `sandbox_exec`, `sandbox_write_file`, `sandbox_read_file`, `sandbox_list_files`, `sandbox_status`, `sandbox_pause`, `sandbox_resume`, `sandbox_terminate`, `sandbox_extend_balance`, `wallet_balance`, `wallet_deposit`, `wallet_deposit_status` — authoritative JSON Schemas: [`internal/mcp/tools.go`](internal/mcp/tools.go).

Notifications (no JSON-RPC `id` / `null` id): **204 No Content**.

---

### `GET /v1/user/profile`

**Auth:** JWT  
**Purpose:** Dashboard user row.

**Response 200:** [`User`](internal/store/store.go).  
**Errors:** `unauthorized` (401), `not_found` (404).

---

### `GET /v1/user/credits`

**Auth:** JWT  
**Purpose:** Fiat credit balance.

**Response 200:**

```json
{ "credit_balance_usd_cents": 0 }
```

**Errors:** `unauthorized` (401), `not_found` (404).

---

### `GET /v1/user/credits/history`

**Auth:** JWT  
**Purpose:** Recent credit ledger rows.

**Response 200:** Array of [`CreditTxSummary`](internal/store/store.go) (up to **100**).  
**Errors:** `unauthorized` (401), `store_error` (500).

---

### `POST /v1/user/api-keys`

**Auth:** JWT  
**Purpose:** Issue a new API key.

**Request body (optional):**

```json
{ "name": "optional-label" }
```

Empty or missing name defaults to `"default"`.

**Response 200:**

```json
{
  "key": "<plaintext sockt_live_… shown once>",
  "id": "<uuid>",
  "prefix": "<short_prefix>",
  "name": "<name>"
}
```

**Errors:** `unauthorized` (401), `store_error` (500).

---

### `GET /v1/user/api-keys`

**Auth:** JWT  
**Purpose:** List keys without secret material.

**Response 200:** Array of `{ "id", "prefix", "name", "is_active" }`.

**Errors:** `unauthorized` (401), `store_error` (500).

---

### `DELETE /v1/user/api-keys/{id}`

**Auth:** JWT  
**Purpose:** Revoke an API key by UUID (`id` path parameter).

**Response:** **204 No Content** on success.  
**Errors:** `unauthorized` (401), `bad_request` (400 invalid id), `revoke_failed` (400).

---

### `POST /v1/wallet/offer`

**Auth:** JWT  
**Purpose:** Ensure a Lightning **`Agent`** row exists for `sub` ([`JWTSub`](internal/store/store.go) / dashboard user linkage). Accepts arbitrary JSON body (decoded but not required for logic).

**Response 200:** [`Agent`](internal/store/store.go) — existing or newly created.  
**Errors:** `unauthorized` (401), `store_error` (500).

---

### `GET /v1/wallet/balance`

**Auth:** JWT  
**Purpose:** Prepaid Lightning balance and sizing hints ([`composeDepositHintURL`](internal/api/wallet_lightning.go), [`lightningwallet.Hints`](internal/lightningwallet/hints.go)).

**Response 200 (fields include):**

- `agent_id`, `balance_msats`, `warning_level`, `next_action`, `min_deposit_msats`, `suggested_deposit_msats`, `deposit_url`, `billing_hint_msats_sec`.

**Errors:** `unauthorized` (401), `not_found` (404 if agent not registered), `store_error` (500).

---

### `POST /v1/wallet/deposit`

**Auth:** JWT  
**Purpose:** Mint a BOLT11 invoice to top up prepaid Lightning balance.

**Request body:**

```json
{
  "amount_sats": 0,
  "idempotency_key": "optional-stable-string"
}
```

**Response 200:** `bolt11`, `payment_hash` (hex), `amount_msats`, `expires_at` (RFC3339), `min_amount_msats`.

**Errors:** `unauthorized` (401), `payment_disabled` (503 if LND missing), `not_found` (404 if no agent), `invoice_failed` (502), `invoice_bad_hash` (502).

---

### `GET /v1/wallet/deposits/{payment_hash}`

**Auth:** JWT  
**Purpose:** Poll deposit invoice status (`payment_hash` path segment: **64 hex chars**, 32 bytes).

**Response 200:** `status`, `amount_msats`, `settled_at` (RFC3339 or null), `bolt11` (optional).  
**Errors:** `unauthorized` (401), `not_found`, `bad_request` (bad hash), `store_error`.

---

### `POST /v1/sandbox/create`

**Auth:** API key **or** L402 ( [`eitherAPIKeyOrL402`](internal/api/router.go)).

**Request body:**

```json
{
  "billing_method": "lightning | credits",
  "tier": "nano | micro | standard | gpu_small | gpu_large"
}
```

**Rules:**

- **`credits`:** Caller must authenticate as API key principal; binds `UserID` / `APIKeyID`.
- **`lightning`:** Caller must authenticate via L402; binds `AgentID`.
- Validates tier pricing and RunPod tier mapping (`RUNPOD_TEMPLATE_*` env IDs); allocates credits server-side when applicable.

**Response 200:** Full [`Sandbox`](internal/store/store.go) row after orchestrator provisioning (reloaded). Stored `Template` reflects the canonical label (`sockt-cpu`, `sockt-gpu-small`, `sockt-gpu-large`), not client input.

**Errors (non-exhaustive):** `unauthorized`, `bad_request` (JSON, billing_method, tier), `bad_tier`, `insufficient_credits` (402 credits path), `orchestrator_disabled` (503 RunPod templates misconfigured / unknown tier mapping), `runpod_error` (502), `store_error` (500).

---

### `GET /v1/sandbox/{id}`

**Auth:** API key or L402.  
**Path:** `{id}` = sandbox UUID.

**Response 200:** [`Sandbox`](internal/store/store.go).  
**Errors:** `bad_request`, `not_found`, `forbidden`.

---

### `POST /v1/sandbox/{id}/pause`

**Auth:** API key or L402.  
**Purpose:** Pause RunPod workload.

**Response:** **204 No Content**.

**Errors:** `bad_request`, `forbidden`, `pause_failed` (502).

---

### `POST /v1/sandbox/{id}/resume`

**Auth:** API key or L402.

**Response:** **204 No Content**.

**Errors:** `bad_request`, `forbidden`, `resume_failed` (502).

---

### `DELETE /v1/sandbox/{id}`

**Auth:** API key or L402.  
**Purpose:** Terminate sandbox / pod.

**Response:** **204 No Content**.

**Errors:** `bad_request`, `forbidden`, `terminate_failed` (502).

---

### `POST /v1/sandbox/{id}/refresh-data-jwt`

**Auth:** API key or L402.  
**Purpose:** Mint fresh **data-plane** JWT.

**Response 200:**

```json
{
  "data_jwt": "<jwt>",
  "expires_in_secs": 0
}
```

**Errors:** `forbidden`, `pod_missing`, `token_error`.

---

### `POST /v1/sandbox/{id}/extend-balance`

**Auth:** API key or L402.

**Request body:**

```json
{ "cents": 0 }
```

**Credits sandboxes (`billing_method == "credits"`):**  
`cents` is **USD cents** to allocate upward; if omitted/`<= 0`, defaults from server config (`LowCreditThresholdCents * 10`).

```json
{ "allocated_cents": 0, "balance_after_cents": 0 }
```

**Lightning sandboxes:** The same JSON field **`cents` is overloaded** — the handler reads it into a variable treated as **`sats`** (satoshis) for invoice sizing (`<= 0` defaults to **50_000** sats). Response includes invoice fields (`bolt11`, `payment_hash`, …) and `warning_level`.

**Errors:** `forbidden`, `unauthorized`, `insufficient_credits` (402), invoice / payment classification errors (`bad_gateway`-class), etc.

---

### `GET /v1/sandbox/{id}/billing`

**Auth:** API key or L402.  
**Purpose:** Billing interval history for Lightning agents (sandbox must have `AgentID`).

**Response 200:**

```json
{ "intervals": [] }
```

If `sandbox.AgentID` nil: **`intervals` is empty**. Otherwise up to **100** rows from [`ListBillingIntervalsByAgent`](internal/store/billing.go) (each [`BillingInterval`](internal/store/store.go): `SandboxID`, `AgentID`, `Start`, `End`, `Msats` — time fields as JSON-marshaled from interfaces).

---

### `POST /v1/runtime/billing/audit-batch`

**Auth:** `Authorization: Bearer <CALLBACK_SECRET>`

**Purpose:** Ingress batched metering from runtime pods.

**Request body:**

```json
{
  "sandbox_id": "<uuid>",
  "pod_id": "<string>",
  "billing_method": "lightning | credits",
  "intervals": [
    {
      "started_at": "<RFC3339>",
      "ended_at": "<RFC3339>",
      "rate_per_second": 0,
      "consumed": 0,
      "billing_method": "..."
    }
  ]
}
```

Interval layout matches [`runtime.AuditInterval`](internal/runtime/callbacks.go). `pod_id` must match stored sandbox `PodID` (case-insensitive trim).

**Response:** **204 No Content** on success.

**Errors (JSON via `writeErr`):** `bad_request`, `not_found`, `pod_replaced` (403), `store_error`, `tier_error`, etc.

---

### `POST /v1/runtime/topup-trigger`

**Auth:** `Authorization: Bearer <CALLBACK_SECRET>`

**Purpose:** Runtime requests fiscal top-up (credits allocate or Lightning deposit invoice paths).

**Request body:**

```json
{
  "sandbox_id": "<uuid>",
  "pod_id": "<string>",
  "billing_method": "lightning | credits",
  "balance": 0,
  "consumed_total": 0,
  "reason": "optional",
  "requested_topup": 0
}
```

**Semantics:** Matches [`triggerPayload`](internal/api/runtime_handlers.go). Responses are often **HTTP 200** with JSON payloads that include `balance_delta`, `error_class`, and Lightning `deposit / bolt11 / payment_hash…` wrappers — **`error_class`** discriminates flows (`deposit_required`, `insufficient_credits`, `deposit_mint_failed`, etc.; see handler).

---

### `POST /v1/runtime/pause-trigger`

**Auth:** `Authorization: Bearer <CALLBACK_SECRET>`

**Purpose:** Out-of-band pause orchestration (`triggerPayload` sans some fields respected).

**Response:** **204** on success.  
**Errors:** `bad_request`, `not_found`, `pod_replaced` (403), `pause_failed` (502).

Writes an audit row on success.

---

### `POST /internal/webhooks/stripe`

**Auth:** Stripe signature (`Stripe-Signature`).

**Purpose:** Processes **`payment_intent.succeeded`**. Credits user when `metadata.user_id` parses as UUID ([`AddUserCredits`](internal/store/users.go)); amount from Stripe `amount_received`.

**Response:** **200 OK** empty body when event ignored successfully; **`503`** `stripe not configured` if webhook secret absent; **`400`** for bad JSON/signature (**JSON** `{ code: "bad_signature"|"bad_json"|…}`); **`500`** `credit_failed` if DB ledger write fails.

---

## Quick route table

| Method | Path | Auth |
|--------|------|------|
| GET | `/healthz` | Public |
| GET | `/v1/pricing` | Public |
| GET | `/v1/mcp/health` | Public |
| POST | `/v1/mcp` | API key / L402 / JWT |
| GET | `/v1/user/profile` | JWT |
| GET | `/v1/user/credits` | JWT |
| GET | `/v1/user/credits/history` | JWT |
| POST | `/v1/user/api-keys` | JWT |
| GET | `/v1/user/api-keys` | JWT |
| DELETE | `/v1/user/api-keys/{id}` | JWT |
| POST | `/v1/wallet/offer` | JWT |
| GET | `/v1/wallet/balance` | JWT |
| POST | `/v1/wallet/deposit` | JWT |
| GET | `/v1/wallet/deposits/{payment_hash}` | JWT |
| POST | `/v1/sandbox/create` | API key or L402 |
| GET | `/v1/sandbox/{id}` | API key or L402 |
| POST | `/v1/sandbox/{id}/pause` | API key or L402 |
| POST | `/v1/sandbox/{id}/resume` | API key or L402 |
| DELETE | `/v1/sandbox/{id}` | API key or L402 |
| POST | `/v1/sandbox/{id}/refresh-data-jwt` | API key or L402 |
| POST | `/v1/sandbox/{id}/extend-balance` | API key or L402 |
| GET | `/v1/sandbox/{id}/billing` | API key or L402 |
| POST | `/v1/runtime/billing/audit-batch` | Bearer callback secret |
| POST | `/v1/runtime/topup-trigger` | Bearer callback secret |
| POST | `/v1/runtime/pause-trigger` | Bearer callback secret |
| POST | `/internal/webhooks/stripe` | Stripe-Signature |

To verify parity with code: `grep -n 'mux.Handle' internal/api/router.go`.
