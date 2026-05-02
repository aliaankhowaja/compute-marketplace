# Component 1: API Gateway & Native L402

## v2 changelog

- Aperture sidecar removed entirely. L402 verification (macaroon HMAC chain + preimage check + caveat enforcement) is now native Go middleware in the control-plane binary at `internal/auth/l402.go`.
- The gateway no longer terminates auth at a separate process; the GCP global LB simply load-balances HTTPS to the control-plane internal LB or the host-data internal LB based on URL path.
- Added explicit path split: `/v1/*` and `/mcp/*` go to the control plane; `/h/{host_id}/*` goes to the per-host data plane via `ilb-host-data`. Data-plane requests carry a short-lived `data_jwt` minted by the control plane after the initial auth, so the host-agent verifies it locally without contacting the control plane.
- Removed the `X-Auth-Bypass: api_key` indirection — there is no separate proxy to inform; one Go middleware sees the request and dispatches by `Authorization` scheme.
- Pre-minted L402 BOLT11 invoice pool eliminates `lnd.AddInvoice` from the 402 response path.

---

## Overview

The "API Gateway" is now just two pieces:

1. **GCP Global HTTPS LB** in front, with HTTP/3, Cloud Armor, and a URL map that routes by path prefix.
2. **Native Go middleware** inside the control-plane binary that validates L402, JWT, and API key credentials in a single pass.

There is no Aperture process, no Nginx, no sidecar. The control plane is the only thing that authenticates external traffic. Once a sandbox is created, host-data-plane traffic is authenticated by the host-agent itself using a JWT that the control plane signed at sandbox-create time.

---

## Responsibilities

- Terminate HTTPS at the global LB (managed certs, HTTP/3, anycast)
- Rate-limit by IP at Cloud Armor (DDoS layer)
- Rate-limit per-agent and per-API-key in Go middleware
- Route `/v1/*` and `/mcp/*` to the control-plane internal LB
- Route `/h/{host_id}/*` to the host-data internal LB
- Inside the control plane: dispatch requests to the L402, API-key, or JWT validator based on the `Authorization` scheme
- Issue L402 macaroons backed by a pre-minted BOLT11 invoice pool on first unauthenticated request to L402-gated routes
- Mint a `data_jwt` (sandbox-scoped, short-lived) when a sandbox is created or resumed; embed in the response for the SDK to use against the host data plane
- Verify, on the host-agent side, the `data_jwt` locally (no control-plane round-trip)
- Return structured JSON error responses

---

## Technology Choices

| Component | Technology | Reason |
|-----------|------------|--------|
| Edge LB | GCP Global External HTTPS LB | Anycast, managed certs, HTTP/3 native, integrated Cloud Armor |
| Native L402 verification | Go (own implementation, ports `gopkg.in/macaroon.v2` HMAC chain + preimage check) | Eliminates Aperture sidecar, ~1-2ms saved per request, single auth code path |
| Rate limiting | Cloud Armor (IP) + Go middleware (per-agent / per-key) | One layer at the edge, one in-process for fine-grained limits |
| Lightning node backend | LND HA pair (Component 9) + LNDK | BOLT12 onion messaging without leaving LND |
| Macaroon library | `gopkg.in/macaroon.v2` (Go) | Standard implementation; matches Aperture's macaroon format for compatibility with existing L402 wallets |

---

## Authentication Methods

The control-plane binary supports three distinct auth schemes, all dispatched by `internal/auth/middleware.go` looking at the `Authorization` header.

| Method | Who Uses It | Header | Validated By | Billing |
|---|---|---|---|---|
| **L402 + Macaroon** | Lightning agents (programmatic) | `Authorization: L402 <macaroon>:<preimage>` | `internal/auth/l402.go` (stateless crypto + caveat check) | Prepaid msat balance (top-ups via BOLT12 — see Component 3) |
| **API Key** | Website users' agents (MCP, SDK) | `Authorization: Bearer sockt_live_...` | `internal/auth/apikey.go` (LRU cache → Cloud SQL) | USD-cent credit deduction from prepaid balance |
| **Website User JWT** | Website dashboard calls to `/v1/user/*` | `Authorization: Bearer <supabase_jwt>` | `internal/auth/jwt.go` (stateless JWT verify against Supabase JWKS) | N/A (key/credit management only) |

All three resolve to a `principal` struct that downstream handlers consume:

```
type Principal struct {
    Kind        AuthKind   // L402 | APIKey | UserJWT
    AgentID     uuid.UUID  // for L402
    UserID      uuid.UUID  // for APIKey, UserJWT
    APIKeyID    uuid.UUID  // for APIKey
    Caveats     map[string]string // for L402
    BalanceKey  string     // hashed identity used for in-memory balance lookup
}
```

There is no `X-Macaroon-Verified` style header; the auth result lives only in the request context.

---

## Route Classification

### L402-Gated Routes

The middleware applies L402 verification when `Authorization: L402 ...` is present, or returns 402 if no `Authorization` header is set on these routes.

| Route Prefix | Initial Price (sats) | Macaroon Capabilities | Notes |
|---|---|---|---|
| `POST /v1/wallet/offer` | 1 sat | `service=onboarding` | One-time: agent registers BOLT12 offer |
| `POST /v1/sandbox/create` | 0 sat (top-up funded) | `service=sandbox`, `tier=<tier>` | Tier caveat written at creation time; first top-up triggered server-side |
| `POST /v1/sandbox/{id}/extend-balance` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Manual top-up trigger |
| `GET /v1/billing/history` | 0 sat | `service=billing` | Billing ledger read |
| `POST /v1/sandbox/{id}/delegate` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Macaroon attenuation for sub-agents |

All sandbox data-plane operations (`exec`, `read_file`, `write_file`, `shell`, `status`, `pause`, `resume`, `terminate`) are NOT on this list because they live on the host-data plane (`/h/{host_id}/sandbox/{id}/*`) and use `data_jwt` instead.

### Public Routes

| Route | Notes |
|---|---|
| `GET /v1/pricing` | Pricing information |
| `GET /v1/templates` | Template catalog |
| `GET /healthz` | Health check |

### Website User Routes (JWT only)

| Method | Route | Description |
|---|---|---|
| `POST` | `/v1/user/api-keys` | Create a new API key |
| `GET` | `/v1/user/api-keys` | List all API keys |
| `PATCH` | `/v1/user/api-keys/{id}` | Update key metadata |
| `DELETE` | `/v1/user/api-keys/{id}` | Revoke an API key |
| `GET` | `/v1/user/profile` | Get website user profile |
| `GET` | `/v1/user/credits` | Get credit balance |
| `GET` | `/v1/user/credits/history` | Credit transaction history |

### Host Data Plane Routes (data_jwt only, terminated on host-agent)

| Method | Route | Description |
|---|---|---|
| `POST` | `/h/{host_id}/sandbox/{id}/exec` | Execute a command |
| `POST` | `/h/{host_id}/sandbox/{id}/files` | Write a file |
| `GET` | `/h/{host_id}/sandbox/{id}/files` | Read a file |
| `GET` | `/h/{host_id}/sandbox/{id}/files/list` | List files |
| `GET` | `/h/{host_id}/sandbox/{id}/status` | Live status (CPU, mem, balance) |
| `GET` | `/h/{host_id}/sandbox/{id}/shell` (WSS) | Interactive shell |

These never reach the control plane (the global LB routes them straight to `ilb-host-data`).

---

## Native L402 Middleware Design

### Macaroon Format

Identical to Aperture's macaroon format so existing L402-aware wallets (Lexe, Alby, Voltage) interoperate without change. A macaroon contains:

- `id`: random 16-byte identifier (the platform's "key id" — used to look up the HMAC root key in Secret Manager)
- `payment_hash`: 32-byte SHA-256 of the BOLT11 preimage that funds this macaroon
- `caveats`: array of first-party caveats (`service=...`, `sandbox_id=...`, `expiry=...`, `readonly=...`, `max_duration_secs=...`)
- `signature`: HMAC chain signature

### Verification Steps (per request)

1. Parse `Authorization: L402 <macaroon_b64>:<preimage_hex>`
2. Decode the macaroon
3. Look up the HMAC root key for the macaroon's `id` from a per-process LRU cache (refilled from Secret Manager on miss)
4. Recompute the HMAC chain over `id` + each caveat; compare to the macaroon's signature → reject on mismatch
5. Compute `sha256(preimage_hex)`; compare to `payment_hash` → reject on mismatch
6. Walk the caveats and reject if:
   - `expiry` < now
   - `service` does not match the requested route's service tag
   - `sandbox_id` does not match the URL `{id}`
   - `readonly=true` and the request method is not GET
   - `max_duration_secs` exceeded since macaroon issuance
7. Resolve `agent_id` from the macaroon's `id` → put on request context as `Principal{Kind: L402, AgentID: ...}`
8. Continue to the handler

Total verification cost: one HMAC, one SHA-256, ~10 string comparisons. < 100µs in benchmarks.

### Pre-Minted BOLT11 Invoice Pool

To remove the 50-200ms `lnd.AddInvoice` call from every 402 response, the payment module maintains an in-memory pool of unused BOLT11 invoices:

- Pool target size: 1000 invoices
- Refill: background goroutine creates new invoices whenever the pool drops below 500
- Each invoice expires in 1 hour; expired invoices are evicted
- Each invoice has a fixed amount (1 sat) for L402 onboarding; per-route price differences are handled by minting on demand for non-onboarding routes (rare)

When a 402 response is needed, the middleware pulls one invoice from the pool, builds the macaroon binding `payment_hash = sha256(preimage)` (the preimage is generated when the invoice was minted and stored alongside it), and returns both. P50 latency for 402 responses drops from ~150ms to ~5ms.

---

## L402 Authentication Flow — Step by Step

### Step 1: Initial Unauthenticated Request

Agent sends `POST /v1/sandbox/create` without an `Authorization` header.

The control-plane middleware responds:

```
HTTP/1.1 402 Payment Required
WWW-Authenticate: L402 token="<base64_macaroon>", invoice="<BOLT11_invoice>"
Content-Type: application/json

{
  "code": 402,
  "error": "payment_required",
  "token": "<base64_macaroon>",
  "invoice": "<BOLT11_invoice>",
  "amount_sats": 1,
  "message": "Pay the invoice and retry with Authorization: L402 <token>:<preimage>"
}
```

(The macaroon and invoice come from the pre-minted pool; total handler latency for this response is ~5ms.)

### Step 2: Agent Pays Invoice

The agent pays via its Lexe wallet (or any L402-compatible wallet) and obtains the preimage.

### Step 3: Authenticated Retry

```
Authorization: L402 <base64_macaroon>:<hex_preimage>
```

### Step 4: Native Middleware Verifies

Steps 1–8 from "Verification Steps" above. On success, the request proceeds to `HandleCreateSandbox`.

### Step 5: Top-Up Triggered Server-Side

`HandleCreateSandbox` calls `payment.TopUpBalance(agent_id, initial_chunk_sats)` which uses the macaroon binding to identify the agent's BOLT12 offer and pulls the initial balance. If this fails, the sandbox creation rolls back. If it succeeds, the sandbox starts with a positive balance and the billing engine takes over (Component 3).

---

## API Key Authentication Flow — Step by Step

### Step 1: Agent Sends API Key

```
POST /v1/sandbox/create
Authorization: Bearer sockt_live_aB3xK9mPqR7vNwYzL2cD5eF8gH1jI4k
Content-Type: application/json
```

### Step 2: Native Middleware Validates

The single `AuthMiddleware` sees `Bearer sockt_live_...` and dispatches to `internal/auth/apikey.go`:

1. Extract the raw key
2. Compute `key_hash = SHA-256(raw_key)`
3. Check the per-instance LRU cache for `key_hash`:
   - **Cache hit:** use cached `{user_id, allowed_tiers, is_active, expires_at, ip_allowlist, rate_limit_rpm}`
   - **Cache miss:** query Cloud SQL `api_keys` table by `key_hash` and populate cache
4. Validate active/expiry/IP allowlist/tier
5. Update `last_used_at` asynchronously (fire-and-forget)
6. Set `Principal{Kind: APIKey, UserID: ..., APIKeyID: ...}` on context

### Cache Invalidation Across Instances

API key revocation must propagate sub-second across all control-plane instances. Mechanism:

- `DELETE /v1/user/api-keys/{id}` issues `NOTIFY apikey_revoked, '<api_key_id>'` on Cloud SQL after marking `is_active = false`
- Each control-plane instance holds a `LISTEN apikey_revoked` connection in a dedicated goroutine
- On notification, the local LRU cache evicts every entry whose `api_key_id` matches

This replaces the 60s LRU TTL revocation lag with sub-second invalidation.

### Step 5: Failure Cases

| Condition | Response |
|---|---|
| Key not found in DB | `401 Unauthorized`, `error: invalid_api_key` |
| `is_active = false` | `401 Unauthorized`, `error: api_key_revoked` |
| Key expired | `401 Unauthorized`, `error: api_key_expired` |
| IP not in allowlist | `403 Forbidden`, `error: ip_not_allowed` |
| Tier not in `allowed_tiers` | `403 Forbidden`, `error: tier_not_allowed` |

---

## Macaroon Delegation (Sub-Agent Support)

Unchanged in spirit: the `POST /v1/sandbox/{id}/delegate` endpoint produces an attenuated macaroon. The endpoint runs inside the native L402 middleware now (no Aperture). Delegation rules:

- Holder may only **add** caveats, never remove
- `readonly=true` blocks POST/PUT/DELETE
- `max_duration_secs` restricts the sub-agent's session
- `sandbox_id=<uuid>` pins to a specific sandbox

API-key holders can still call this endpoint to obtain an L402 macaroon for sub-agents; the resulting macaroon is funded by the user's credit balance (a single deduction is recorded for the delegation issuance).

---

## Data-Plane JWT (`data_jwt`)

The control plane mints a `data_jwt` whenever a sandbox transitions to `running` (on create or resume). The agent receives it in the response and includes it on every host-data-plane call.

### `data_jwt` Claims

| Claim | Value |
|---|---|
| `iss` | `sockt-cp` |
| `aud` | `host-data-plane` |
| `sub` | `sandbox_id` |
| `host_id` | the host VM the sandbox lives on |
| `principal_kind` | `l402` or `api_key` |
| `agent_id` or `user_id` | the owner |
| `iat` | issuance time |
| `exp` | iat + 1 hour |
| `scope` | array: `exec`, `files`, `shell`, `status` |
| `readonly` | true if delegated readonly |

Signed with HS256 using `wss-jwt-secret` from Secret Manager. The host-agent loads the same secret at startup and verifies tokens locally — no control-plane round-trip per data-plane request.

### Refresh

The SDK proactively refreshes `data_jwt` when it has < 10 minutes remaining by calling `POST /v1/sandbox/{id}/refresh-data-jwt` against the control plane. The control plane verifies the original L402/API-key auth, looks up the sandbox's current `host_id`, and mints a new `data_jwt`. This also handles the case where the sandbox was resumed onto a different host.

---

## Rate Limiting Rules

### Layer 1: Cloud Armor (IP-level, before LB)

| Rule | Threshold | Action |
|---|---|---|
| Per-IP requests/minute | 300 | Throttle (429) |
| Per-IP requests/minute | 600 | Block (403) |
| Known Tor exit node | Any | Block (403) — optional |

### Layer 2: Control-Plane Middleware (per-principal)

A token bucket per `Principal.BalanceKey` enforced in `internal/auth/ratelimit.go`:

| Rule | L402 Default | API Key Default | Override |
|---|---|---|---|
| Requests per minute | 300 | 300 | `api_keys.rate_limit_rpm` per key |
| Sandbox creates per minute | 20 | 20 | Plan-based |
| Concurrent open sandboxes | 50 | `users.max_concurrent_sandboxes` | Per-user limit for API keys |

Cross-instance coordination uses a Cloud SQL `rate_limit_buckets` table sampled every 5s (good enough for 300 rpm thresholds; sub-second cross-instance precision isn't needed at these limits).

### Layer 3: Host-Agent (per-data_jwt)

| Rule | Default | Override |
|---|---|---|
| Exec calls per second | 10 | Not overrideable |
| Concurrent shell connections | 3 per sandbox | Not overrideable |
| Input frame rate (WSS) | 100/sec | Not overrideable |

---

## Error Response Schema

All authentication and gateway errors return a consistent JSON body:

```
{
  "code": <http_status_int>,
  "error": "<error_slug>",
  "message": "<human readable>",
  "request_id": "<uuid>",
  "retry_after_secs": <int>
}
```

| Error Slug | HTTP Status | Cause |
|---|---|---|
| `payment_required` | 402 | No Authorization header on L402 route; returns invoice |
| `invalid_macaroon` | 401 | Macaroon HMAC invalid or tampered |
| `invalid_preimage` | 401 | Preimage does not match macaroon payment hash |
| `macaroon_expired` | 401 | Expiry caveat exceeded |
| `caveat_violation` | 401 | sandbox_id mismatch or readonly violation |
| `invalid_api_key` | 401 | API key not found or structurally invalid |
| `api_key_revoked` | 401 | API key has been revoked |
| `api_key_expired` | 401 | API key has passed its expiry date |
| `ip_not_allowed` | 403 | Request IP not in API key allowlist |
| `tier_not_allowed` | 403 | Requested tier not in API key allowed_tiers |
| `data_jwt_invalid` | 401 | host-agent rejected the data-plane JWT |
| `data_jwt_expired` | 401 | data_jwt past `exp`; SDK should refresh |
| `host_unreachable` | 503 | host-agent not responding; failover in progress |
| `rate_limited` | 429 | Too many requests |
| `service_unavailable` | 503 | Control plane health check failing |

---

## Headers Set by Middleware (Trusted Internal Headers)

The native middleware places auth context on the Go request context, not on HTTP headers — there is no longer an HTTP boundary between auth and handler. The only HTTP headers that survive into handlers are:

| Header | Content |
|---|---|
| `X-Request-ID` | UUID generated per request |

External clients cannot influence the in-process Principal struct.

---

## MCP Server Configuration (API Key Path)

Website users who set up an MCP server configure it with their API key:

```json
{
  "mcpServers": {
    "sockt": {
      "command": "npx",
      "args": ["-y", "@sockt/mcp-server"],
      "env": {
        "SOCKT_API_URL": "https://api.sockt.io",
        "SOCKT_API_KEY": "sockt_live_aB3xK9mPqR7vNwYzL2cD5eF8gH1jI4k"
      }
    }
  }
}
```

When `SOCKT_API_KEY` is set, the MCP proxy sends `Authorization: Bearer sockt_live_...` on all control-plane requests and skips the L402 + Lexe wallet payment flow entirely. After `sandbox.create`, the proxy starts using the returned `data_endpoint` + `data_jwt` for tool calls that need data-plane reach (`exec`, `write_file`, etc.).

---

## Deployment Notes

There is no per-service-binary sidecar to deploy. The control plane is a single Go binary on `mig-control-plane`:

- Binds plain HTTP on `:8081` (the internal LB terminates TLS for the control path; mTLS isn't required between the LB and the MIG since traffic is already in-VPC)
- Loads `wss-jwt-secret`, `host-agent-mtls-ca` (client cert for talking to host-agents), `cloudsql-password`, `lnd-tls-cert`, `lnd-invoice-macaroon`, `supabase-jwt-secret` from Secret Manager at startup
- Connects to Cloud SQL primary via private IP at `10.4.0.3:5432`
- Connects to active LND via private IP at `10.0.0.5:10009` (with failover discovery via the `lnd-active` GCE label)
- Joins the cluster gossip on `:7444`

---

## Observability

### Metrics (Prometheus, scraped by Cloud Monitoring)

| Metric | Type | Labels |
|---|---|---|
| `auth_l402_verify_duration_us` | Histogram | result (ok/fail) |
| `auth_l402_402_issued_total` | Counter | route |
| `auth_l402_invoice_pool_size` | Gauge | — |
| `auth_l402_invoice_pool_misses_total` | Counter | — |
| `auth_apikey_lookup_duration_ms` | Histogram | source (cache/db) |
| `auth_apikey_cache_hit_ratio` | Gauge | — |
| `auth_apikey_revocation_propagation_ms` | Histogram | — |
| `auth_jwt_verify_duration_us` | Histogram | — |
| `auth_data_jwt_minted_total` | Counter | scope |

### Structured Log Fields (every request)

- `request_id` — UUID
- `agent_id` or `user_id` — set after auth
- `auth_method` — `l402`, `api_key`, `user_jwt`, `data_jwt`, or `unauthenticated`
- `api_key_id` — if API key auth
- `route` — path + method
- `auth_result` — `pass`, `fail_preimage`, `fail_caveat`, `fail_expiry`, `invalid_api_key`, `api_key_revoked`, `unauthenticated`
- `status_code`
- `duration_ms`

---

## Security Considerations

- The macaroon HMAC root key is loaded once at startup and held only in memory. Rotation requires a rolling restart and a key-id bump.
- LND macaroon used by the payment module is scoped to `invoice:read`, `invoice:write`, `offchain:read`, `offchain:write` — never an admin macaroon.
- All secrets (LND macaroon, TLS cert, JWT secrets) are mounted from GCP Secret Manager via the metadata server — never written to disk on the GCE instance.
- Logs scrub macaroon and preimage values (truncated to 8 chars).
- Raw API keys never appear in logs. Only `api_key_id` and `key_prefix` (first 12 chars).
- The `key_hash` column has no SELECT policy for any role except the control-plane service account.
- `data_jwt` validation on host-agents uses constant-time HMAC comparison.

---

## Dependencies

- LND HA pair (Component 5: Payment Module) reachable at `10.0.0.5:10009` (with failover to `10.0.0.6`)
- GCP Global LB + Cloud Armor (Component 9: GCP Infrastructure)
- Control-plane binary (Component 2) — auth lives inside it
- Cloud SQL (Component 6) for `api_keys`, `agents`, and `rate_limit_buckets` tables
- GCP Secret Manager for all key material
- Per-host `host-agent` binaries (Component 4) verify `data_jwt` independently
