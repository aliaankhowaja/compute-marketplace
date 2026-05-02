# Component 1: API Gateway & L402 Proxy

## Overview

The API Gateway is the single public entry point for all traffic into Satoshi Compute. It consists of two layers running as sidecars on the same Cloud Run service: an **Nginx reverse proxy** handling TLS termination, rate limiting, and request routing, and an **Aperture L402 proxy** that gates every protected route behind a Lightning payment + macaroon credential check.

No business logic lives here. The gateway's sole responsibilities are authentication enforcement, traffic shaping, and routing to the correct Go handler inside the unified binary.

---

## Responsibilities

- Terminate HTTPS and enforce TLS 1.3 minimum
- Rate-limit by IP and by agent identity (macaroon root key or API key hash)
- Route `/v1/*` to the Go API module
- Route `/mcp/*` to the Go MCP module
- Gate L402-protected routes through Aperture (Lightning agents)
- Pass API-key-authenticated requests directly to the Go binary, bypassing Aperture (Go binary validates the key)
- Pass website user JWT requests directly to the Go binary for `/v1/user/*` routes (no L402 required)
- Issue BOLT12-backed macaroons on first unauthenticated request to L402 routes (HTTP 402)
- Verify macaroon + preimage on subsequent authenticated L402 requests
- Forward verified agent identity headers to the Go binary
- Return structured JSON error responses for auth failures
- Log every request with sandbox_id and agent_id trace context

---

## Technology Choices

| Component | Technology | Reason |
|-----------|------------|--------|
| L402 proxy | Aperture (Lightning Labs, Go) | Reference implementation; Aperture-native macaroon minting; integrates directly with LND gRPC |
| TLS termination | GCP Cloud Load Balancer (upstream of Cloud Run) | Managed certs, anycast, no config needed |
| Rate limiting | Aperture built-in + Cloud Armor (GCP) + Go middleware | Aperture rate-limits L402 sessions per macaroon; Go middleware rate-limits API key sessions; Cloud Armor rate-limits by IP |
| Lightning node backend | LND 0.18+ with LNDK sidecar | Aperture requires LND; LNDK adds BOLT12 onion messaging |

---

## Authentication Methods

The gateway supports three distinct authentication paths. All three ultimately resolve to an `agent_id` that is forwarded to the Go binary.

| Method | Who Uses It | Header | Validated By | Billing |
|---|---|---|---|---|
| **L402 + Macaroon** | Lightning agents (programmatic, no account) | `Authorization: L402 <macaroon>:<preimage>` | Aperture sidecar (stateless crypto) | BOLT12 Lightning micropayments |
| **API Key** | Website users' agents (MCP servers, SDK) | `Authorization: Bearer sc_live_...` | Go binary auth middleware (LRU cache → Supabase) | USD credit deduction from pre-purchased balance |
| **Website User JWT** | Website dashboard calls to `/v1/user/*` | `Authorization: Bearer <supabase_jwt>` | Go binary `UserJWTMiddleware` (stateless JWT verify) | N/A (key management only) |

Aperture only sees and processes requests using the L402 path. Requests using an API key or website JWT are **passed through Aperture untouched** — Aperture is configured to allow these routes through unconditionally, and the Go binary is solely responsible for validating them.

---

## Route Classification

### L402-Gated Routes (Aperture enforces payment)

These routes require either a valid L402 credential or an API key. Aperture intercepts requests to these routes. If the request carries `Authorization: L402 ...`, Aperture verifies it. If the request carries `Authorization: Bearer sc_live_...`, Aperture is configured to forward it to the Go binary with an `X-Auth-Bypass: api_key` header, allowing the Go binary to handle validation.

| Route Prefix | Initial Price (sats) | Macaroon Capabilities | Notes |
|---|---|---|---|
| `POST /v1/wallet/offer` | 1 sat | `service=onboarding` | One-time: agent registers BOLT12 offer |
| `POST /v1/sandbox/create` | 0 sat (price encoded in tier caveat) | `service=sandbox`, `tier=<tier>` | Tier caveat written at creation time |
| `POST /v1/sandbox/{id}/exec` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Macaroon scoped to specific sandbox |
| `GET /v1/sandbox/{id}/status` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Read-only, same macaroon |
| `POST /v1/sandbox/{id}/files` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Write scoped to sandbox |
| `GET /v1/sandbox/{id}/files` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Read scoped to sandbox |
| `POST /v1/sandbox/{id}/pause` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Lifecycle op |
| `POST /v1/sandbox/{id}/resume` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Lifecycle op |
| `DELETE /v1/sandbox/{id}` | 0 sat | `service=sandbox`, `sandbox_id=<id>` | Termination |
| `GET /v1/billing/history` | 0 sat | `service=billing` | Billing ledger read |

### Public Routes (no auth, Aperture passes through)

| Route | Notes |
|---|---|
| `GET /v1/pricing` | Pricing information |
| `GET /v1/templates` | Template catalog |
| `GET /healthz` | Health check |

### Website User Routes (JWT-only, Aperture passes through)

These routes are excluded from Aperture's payment gate entirely. They are validated by the Go binary's `UserJWTMiddleware` using the Supabase Auth JWT from the website user's session.

| Method | Route | Description |
|---|---|---|
| `POST` | `/v1/user/api-keys` | Create a new API key |
| `GET` | `/v1/user/api-keys` | List all API keys for the authenticated user |
| `PATCH` | `/v1/user/api-keys/{id}` | Update key metadata (name, description, limits) |
| `DELETE` | `/v1/user/api-keys/{id}` | Revoke an API key |
| `GET` | `/v1/user/profile` | Get website user profile and linked agent summary |

Aperture configuration explicitly excludes the `/v1/user/*` prefix from its service definitions. Requests to these routes bypass Aperture's invoice check entirely and are forwarded directly to the Go binary on `127.0.0.1:8081`.

---

## Aperture Configuration

### Service Definition

Aperture is configured with a services file that maps URL path prefixes to backend addresses and pricing rules. Each service entry declares:

- **Host pattern** — the URL prefix to intercept
- **Backend URL** — where to forward authenticated requests (the Go binary's internal port)
- **Auth type** — `lnd` (Lightning invoice + macaroon)
- **Price** — initial access price in satoshis
- **Capabilities** — list of macaroon caveats Aperture will enforce
- **Bypass header** — when `Authorization: Bearer sc_live_...` is detected, Aperture adds `X-Auth-Bypass: api_key` and forwards without issuing a 402

### Aperture Bypass Logic for API Keys

Aperture is configured with a **header-based bypass rule** for API key traffic:

- If the incoming `Authorization` header value starts with `Bearer sc_live_` or `Bearer sc_test_`: forward the request to the Go binary with `X-Auth-Bypass: api_key` set, without issuing a 402 or verifying a macaroon.
- The Go binary's `APIKeyMiddleware` then performs the actual key validation.
- If validation fails in the Go binary, it returns `401 Unauthorized` directly — Aperture is not involved in API key rejection.

This design keeps Aperture as the sole L402 authority while adding API key support purely in Go without modifying Aperture's core logic.

### Macaroon Caveats Enforced by Aperture

Aperture enforces caveats at the proxy layer before the request reaches Go code. The Go binary trusts headers Aperture sets — it does not re-verify the macaroon. Caveats do not apply to API key requests.

| Caveat Key | Example Value | Enforcement Rule |
|---|---|---|
| `service` | `sandbox` | Request path must match service category |
| `tier` | `micro` | Only used for billing pre-authorization logging |
| `sandbox_id` | `uuid` | `{id}` in the URL path must equal this value |
| `max_duration_secs` | `3600` | Aperture checks wall clock against macaroon issue time |
| `expiry` | `unix timestamp` | Hard expiry; Aperture rejects expired macaroons |
| `readonly` | `true` | Aperture blocks POST/PUT/DELETE for read-only delegated macaroons |

---

## L402 Authentication Flow — Step by Step

### Step 1: Initial Unauthenticated Request

Agent sends any `POST /v1/sandbox/create` without an `Authorization` header.

Aperture intercepts and responds:

```
HTTP/1.1 402 Payment Required
WWW-Authenticate: L402 token="<base64_macaroon>", invoice="<BOLT12_or_BOLT11_invoice>"
Content-Type: application/json

{
  "code": 402,
  "error": "payment_required",
  "token": "<base64_macaroon>",
  "invoice": "<BOLT12_offer_or_BOLT11_invoice>",
  "amount_sats": 1,
  "message": "Pay the invoice and retry with Authorization: L402 <token>:<preimage>"
}
```

### Step 2: Agent Pays Invoice

The agent pays the invoice using its Lexe wallet and receives a preimage (32-byte secret).

### Step 3: Authenticated Retry

Agent retries the original request with:

```
Authorization: L402 <base64_macaroon>:<hex_preimage>
```

### Step 4: Aperture Verifies

Aperture performs stateless verification:
1. Decode the macaroon — check HMAC chain integrity
2. Verify the payment hash in the macaroon matches `sha256(preimage)`
3. Check all caveats (expiry, service, sandbox_id)
4. If all pass: forward request to Go binary with `X-Macaroon-Verified: true` and `X-Agent-Caveats: <json>`
5. If any fail: return `HTTP 401 Unauthorized` with structured error

### Step 5: Go Binary Trusts Forwarded Headers

The Go `api` module reads:
- `X-Agent-ID` — agent UUID extracted from macaroon root key identifier
- `X-Macaroon-Caveats` — JSON-encoded caveat map for business logic use
- `X-Macaroon-Verified` — must be `true` or request is rejected at Go middleware level

---

## API Key Authentication Flow — Step by Step

### Step 1: Agent Sends API Key

Agent (e.g., MCP server configured by a website user) sends:

```
POST /v1/sandbox/create
Authorization: Bearer sc_live_aB3xK9mPqR7vNwYzL2cD5eF8gH1jI4k
Content-Type: application/json
```

### Step 2: Aperture Bypasses

Aperture detects the `Bearer sc_live_` prefix, adds `X-Auth-Bypass: api_key` header, and forwards the request unchanged to the Go binary on `127.0.0.1:8081`.

### Step 3: Go Binary APIKeyMiddleware Validates

The Go `APIKeyMiddleware` runs before any route handler:

1. Extracts the raw key from `Authorization: Bearer {raw_key}`
2. Computes `key_hash = SHA-256(raw_key)`
3. Checks the in-process LRU cache for `key_hash`:
   - **Cache hit:** Use cached `{agent_id, allowed_tiers, is_active, expires_at}`
   - **Cache miss:** Query Supabase `api_keys` table by `key_hash`; populate cache
4. Validates:
   - `is_active = true` — key is not revoked
   - `expires_at IS NULL OR expires_at > now()` — key is not expired
   - `ip_allowlist IS NULL OR request_ip IN ip_allowlist` — IP is allowed
   - `allowed_tiers IS NULL OR requested_tier IN allowed_tiers` — tier is permitted
5. Updates `api_keys.last_used_at = now()` asynchronously (fire-and-forget, non-blocking)
6. Sets internal request context: `agent_id`, `auth_method = "api_key"`, `api_key_id`
7. Proceeds to route handler

### Step 4: Route Handler Executes Normally

The route handler (e.g., `HandleCreateSandbox`) reads `agent_id` from request context. It does not know or care whether authentication was via L402 or API key. All billing, orchestration, and audit logic is identical for both paths.

### Step 5: Failure Cases

| Condition | Response |
|---|---|
| Key not found in DB | `401 Unauthorized`, `error: invalid_api_key` |
| Key found but `is_active = false` | `401 Unauthorized`, `error: api_key_revoked` |
| Key expired | `401 Unauthorized`, `error: api_key_expired` |
| IP not in allowlist | `403 Forbidden`, `error: ip_not_allowed` |
| Tier not in allowed_tiers | `403 Forbidden`, `error: tier_not_allowed` |

---

## Macaroon Delegation (Sub-Agent Support)

An orchestrator agent can attenuate its sandbox macaroon and delegate it to a sub-agent with fewer permissions. This applies only to L402-authenticated agents. API key holders may use the `POST /v1/sandbox/{id}/delegate` endpoint to generate a scoped L402 macaroon for sub-agents, even if the parent request was authenticated via API key.

### Delegation Rules

- The holder may only **add** caveats, never remove them
- A macaroon with `readonly=true` allows GET requests only
- A macaroon with `max_duration_secs=300` restricts the sub-agent to 5 minutes
- A macaroon with `sandbox_id=<specific_uuid>` cannot be used on any other sandbox

### Delegation Endpoint (handled by Go api module, not Aperture)

`POST /v1/sandbox/{id}/delegate`

Request body:
- `additional_caveats` — object: key-value pairs to append
- `expiry_secs` — integer: relative expiry for the delegated token

Response:
- `delegated_token` — new base64-encoded attenuated macaroon
- `expiry_at` — absolute UTC timestamp

---

## Rate Limiting Rules

### Layer 1: Cloud Armor (IP-level, before Cloud Run)

| Rule | Threshold | Action |
|---|---|---|
| Per-IP requests/minute | 300 | Throttle (429) |
| Per-IP requests/minute | 600 | Block (403) |
| Known Tor exit node | Any | Block (403) — optional, configurable |

### Layer 2: Aperture (per-macaroon-root-key, L402 only)

| Rule | Threshold | Action |
|---|---|---|
| Exec calls per second | 10 | Throttle (429) |
| Sandbox creations per minute | 20 | Throttle (429) |
| Concurrent open sandboxes | 50 | Reject (429) |

### Layer 3: Go Binary Middleware (API key sessions)

The Go binary applies its own rate limits for API key authenticated requests, using a per-`api_key_id` token bucket stored in-process.

| Rule | Default Threshold | Override |
|---|---|---|
| Requests per minute | 300 | `api_keys.rate_limit_rpm` overrides this per key |
| Exec calls per second | 10 | Not overrideable |
| Sandbox creations per minute | 20 | Not overrideable |
| Concurrent open sandboxes | 50 | Not overrideable |

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
| `rate_limited` | 429 | Too many requests |
| `service_unavailable` | 503 | Go binary health check failing |

---

## Headers Passed to Go Binary (Trusted Internal Headers)

These headers are set by Aperture or by Aperture's bypass logic and must not be accepted from external clients. The Go binary's middleware strips any of these headers that arrive from outside the Aperture trust boundary.

### Set by Aperture for L402 requests

| Header | Content |
|---|---|
| `X-Macaroon-Verified` | `true` |
| `X-Agent-ID` | Agent UUID derived from macaroon root key |
| `X-Agent-Tier` | Tier caveat value (e.g., `micro`) |
| `X-Sandbox-ID` | sandbox_id caveat value, if present |
| `X-Macaroon-Expiry` | Unix timestamp of macaroon expiry |
| `X-Macaroon-Readonly` | `true` if readonly caveat present |
| `X-Request-ID` | UUID generated by Aperture for trace correlation |

### Set by Aperture for API key requests (bypass path)

| Header | Content |
|---|---|
| `X-Auth-Bypass` | `api_key` |
| `X-Request-ID` | UUID generated by Aperture for trace correlation |

The Go binary's `APIKeyMiddleware` reads `X-Auth-Bypass: api_key` and performs its own validation, setting `agent_id` and `auth_method` in the request context internally.

---

## MCP Server Configuration (API Key Path)

Website users who set up an MCP server configure it with their API key instead of a Lexe wallet. The `@satoshi-compute/mcp-server` npm package accepts API key configuration:

```json
{
  "mcpServers": {
    "satoshi-compute": {
      "command": "npx",
      "args": ["-y", "@satoshi-compute/mcp-server"],
      "env": {
        "SATOSHI_API_URL": "https://api.satoshicompute.io",
        "SATOSHI_API_KEY": "sc_live_aB3xK9mPqR7vNwYzL2cD5eF8gH1jI4k"
      }
    }
  }
}
```

When `SATOSHI_API_KEY` is set, the MCP proxy sends `Authorization: Bearer sc_live_...` on all requests and skips the L402 + Lexe wallet payment flow entirely. Compute costs are deducted from the website user's pre-purchased credit balance instead of Lightning payments. No Lightning node or BOLT12 offer is required for API key users.

---

## Deployment on Cloud Run

Aperture runs as a **sidecar container** in the same Cloud Run service revision as the Go binary. The two containers share a loopback network namespace.

### Container Startup Order

1. Go binary starts and binds to `127.0.0.1:8081`
2. Aperture starts and binds to `0.0.0.0:8080` (the public port)
3. Aperture connects to LND gRPC at `10.0.0.5:10009` (VPC internal)
4. Cloud Run health check polls `GET /healthz` on port 8080 (Aperture forwards to Go `/healthz`)

### Environment Variables (Aperture)

| Variable | Value |
|---|---|
| `APERTURE_LND_HOST` | `10.0.0.5:10009` |
| `APERTURE_LND_MACAROON_PATH` | `/secrets/lnd-invoice-macaroon` |
| `APERTURE_LND_TLS_CERT_PATH` | `/secrets/lnd-tls-cert` |
| `APERTURE_BACKEND_URL` | `http://127.0.0.1:8081` |
| `APERTURE_SERVICES_CONFIG` | `/config/aperture-services.yaml` |
| `APERTURE_DEBUG` | `false` |

---

## Observability

### Metrics (exported to Cloud Monitoring)

| Metric | Type | Labels |
|---|---|---|
| `aperture_requests_total` | Counter | route, status_code, auth_result |
| `aperture_402_issued_total` | Counter | route, invoice_type (bolt11/bolt12) |
| `aperture_macaroon_verify_duration_ms` | Histogram | result (ok/fail) |
| `aperture_rate_limit_hits_total` | Counter | rule_name |
| `gateway_api_key_auth_total` | Counter | result (ok/revoked/expired/not_found) |
| `gateway_api_key_cache_hit_ratio` | Gauge | — |

### Structured Log Fields (every request)

- `request_id` — UUID
- `agent_id` — if authenticated (either path)
- `auth_method` — `l402`, `api_key`, `user_jwt`, or `unauthenticated`
- `api_key_id` — if API key auth (key UUID, not the raw key)
- `route` — path + method
- `auth_result` — `pass`, `fail_preimage`, `fail_caveat`, `fail_expiry`, `invalid_api_key`, `api_key_revoked`, `unauthenticated`
- `status_code`
- `duration_ms`

---

## Security Considerations

- Aperture must only accept LND connections over mTLS
- The LND macaroon used by Aperture must be scoped to `invoice:read` and `invoice:write` only — never an admin macaroon
- Internal trust headers (`X-Macaroon-Verified`, `X-Auth-Bypass`, etc.) must be stripped at the Aperture ingress layer if they arrive from external clients
- All secrets (LND macaroon, TLS cert) are mounted from GCP Secret Manager as volume mounts — never environment variables
- Aperture access logs must be shipped to Cloud Logging with no plaintext macaroon values (truncate after first 8 chars)
- Raw API keys must never appear in logs. The Go middleware logs only `api_key_id` (the UUID) and `key_prefix` (first 12 chars of the key)
- The `key_hash` column in Supabase is excluded from all RLS SELECT policies — external Supabase clients cannot read it

---

## Dependencies

- LND node (Component 5: Payment Module) must be running and reachable at `10.0.0.5:10009`
- GCP Cloud Load Balancer (Component 9: GCP Infrastructure) for upstream TLS termination
- Go unified binary (Component 2) must be healthy for Aperture to forward requests
- Supabase `api_keys` table (Component 6) for API key lookup on cache miss
