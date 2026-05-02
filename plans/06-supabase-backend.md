# Component 6: Supabase Backend

## Overview

Supabase serves as the managed backend for all persistent state in Satoshi Compute. It provides PostgreSQL for structured data storage, Row Level Security (RLS) for data isolation, Supabase Auth for agent JWT management, Supabase Realtime for live billing event broadcasting, and Supabase Storage for sandbox filesystem snapshots.

The Go binary connects to Supabase Postgres via the Supavisor connection pooler in transaction mode, which is compatible with Cloud Run's ephemeral container model. The Supabase JavaScript/TypeScript client is used only by external dashboard consumers — the Go binary uses the raw pgx driver.

---

## Services Used

| Supabase Feature | Purpose |
|---|---|
| **Postgres** | Primary database: all entities, billing records, host state, credit ledger |
| **Supabase Auth** | Issues and validates JWTs for agent identity post-L402 registration; also used by website users who sign up to create API keys |
| **Supabase Realtime** | Broadcasts billing_interval inserts and credit balance changes to subscribed dashboard/agent clients |
| **Row Level Security** | Agents and users can only read their own rows — enforced at the DB layer |
| **Supavisor (transaction mode)** | Connection pooling compatible with Cloud Run's transient processes |
| **Supabase Storage** | Snapshot storage bucket for pause/resume |

---

## Billing Model Split

There are two completely separate billing paths depending on how a request is authenticated:

| Auth Method | Billing Method | Proof of Payment | Deducted From |
|---|---|---|---|
| **L402 + BOLT12** (Lightning agents) | BOLT12 pull-pay micropayments every 10 seconds | Lightning preimage stored in `billing_intervals.bolt12_preimage` | Agent's Lightning wallet |
| **API Key** (website users) | Credit deduction from pre-purchased credit balance | Credit ledger row in `credit_transactions` | `users.credit_balance_usd_cents` |

These two paths share the same sandbox, orchestrator, and audit infrastructure but diverge entirely in the billing engine. There is no Lightning involvement for API key sessions.

---

## Database Schema

### Table: agents

Stores one row per agent identity registered via the L402 path (Lightning agents).

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK | Internal agent identifier |
| `jwt_sub` | UUID | UNIQUE, NOT NULL | Supabase Auth user ID |
| `offer_type` | TEXT | NOT NULL | bolt12, lnurl, or lightning_address |
| `bolt12_offer` | TEXT | UNIQUE, nullable | Agent's BOLT12 offer string |
| `lightning_address` | TEXT | UNIQUE, nullable | Fallback Lightning address |
| `lnurl` | TEXT | UNIQUE, nullable | Fallback LNURL |
| `balance_sats` | BIGINT | NOT NULL, DEFAULT 0 | Pre-funded Lightning balance |
| `label` | TEXT | nullable | Agent-assigned label |
| `metadata` | JSONB | nullable | Arbitrary key-value store |
| `created_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | |
| `last_active_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | |
| `total_sats_spent` | BIGINT | NOT NULL, DEFAULT 0 | Lifetime Lightning spend |
| `warning_level` | TEXT | NOT NULL, DEFAULT ok | ok, warning, critical, suspended |

**Indexes:**
- `agents_jwt_sub_idx` ON jwt_sub
- `agents_bolt12_offer_idx` ON bolt12_offer
- `agents_lightning_address_idx` ON lightning_address

---

### Table: users

Stores one row per website user (signed up via email/OAuth). Website users authenticate via Supabase Auth and pay for compute using pre-purchased credits, not Lightning.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, matches auth.users(id) | Supabase Auth user ID — primary key is the auth UUID |
| `email` | TEXT | UNIQUE, NOT NULL | User's email address (from Supabase Auth) |
| `credit_balance_usd_cents` | BIGINT | NOT NULL, DEFAULT 0, CHECK >= 0 | Current credit balance in US dollar cents (e.g., 500 = $5.00) |
| `total_credits_purchased_usd_cents` | BIGINT | NOT NULL, DEFAULT 0 | Lifetime credits purchased |
| `total_credits_consumed_usd_cents` | BIGINT | NOT NULL, DEFAULT 0 | Lifetime credits consumed by sandboxes |
| `plan` | TEXT | NOT NULL, DEFAULT free | free, starter, pro, enterprise |
| `max_concurrent_sandboxes` | INT | NOT NULL, DEFAULT 5 | Concurrent sandbox limit for this user |
| `max_api_keys` | INT | NOT NULL, DEFAULT 10 | Maximum active API keys allowed |
| `created_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | |
| `last_active_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | |
| `warning_level` | TEXT | NOT NULL, DEFAULT ok | ok, low_credits, out_of_credits, suspended |

**Indexes:**
- `users_email_idx` ON email

**Notes:**
- `credit_balance_usd_cents` has a CHECK constraint ensuring it never goes below 0. The billing engine always checks remaining balance before deducting.
- Credits are denominated in USD cents (integer) to avoid floating-point rounding errors in financial calculations.
- Credit-to-compute pricing is defined in the `tier_pricing` table (see below).

---

### Table: tier_pricing

Defines the compute cost per second for each tier in both sats (Lightning path) and USD cents (credit path). This table is the single source of truth for pricing across both billing systems.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `tier` | TEXT | PK | nano, micro, standard, gpu_small, gpu_large |
| `sats_per_second` | NUMERIC(10,4) | NOT NULL | Lightning billing rate |
| `usd_cents_per_second` | NUMERIC(10,6) | NOT NULL | Credit billing rate (e.g., 0.000278 = $0.01/hour) |
| `cold_start_sats` | INT | NOT NULL, DEFAULT 0 | One-time charge (Lightning) for VM provisioning |
| `cold_start_usd_cents` | INT | NOT NULL, DEFAULT 0 | One-time charge (credits) for VM provisioning |
| `updated_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | Last pricing change |

**Example pricing (indicative):**

| Tier | sats/sec | USD cents/sec | USD/hour |
|---|---|---|---|
| nano | 0.5 | 0.000139 | ~$0.50 |
| micro | 2.0 | 0.000556 | ~$2.00 |
| standard | 8.0 | 0.002222 | ~$8.00 |
| gpu_small | 80.0 | 0.022222 | ~$80.00 |
| gpu_large | 400.0 | 0.111111 | ~$400.00 |

---

### Table: api_keys

Stores API keys created by website users. Each key is linked to a `users` row via `user_id`. All sandbox ownership and credit billing flows through `user_id`.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | Internal key record identifier |
| `user_id` | UUID | FK users(id), NOT NULL | The website user who owns this key |
| `key_hash` | TEXT | UNIQUE, NOT NULL | SHA-256 hash of the raw key — stored instead of the key itself |
| `key_prefix` | TEXT | NOT NULL | First 12 chars of raw key for dashboard display (e.g., sc_live_aB3x) |
| `name` | TEXT | NOT NULL | Human label (e.g., My MCP Server) |
| `created_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | |
| `last_used_at` | TIMESTAMPTZ | nullable | Updated on each successful authentication |
| `expires_at` | TIMESTAMPTZ | nullable | Hard expiry; NULL = never expires |
| `is_active` | BOOLEAN | NOT NULL, DEFAULT true | Soft-delete / revoke flag |
| `allowed_tiers` | TEXT[] | nullable | Restrict key to specific tiers; NULL = all tiers |
| `rate_limit_rpm` | INT | nullable | Per-minute request cap override; NULL = plan default |
| `ip_allowlist` | INET[] | nullable | Optional IP whitelist; NULL = all IPs allowed |
| `description` | TEXT | nullable | Optional notes |

**Indexes:**
- `api_keys_key_hash_active_idx` ON (key_hash) WHERE is_active = true — primary lookup path on every auth
- `api_keys_user_id_idx` ON user_id
- `api_keys_key_hash_idx` ON key_hash — for revocation lookup

**Key format:** `sc_live_{32 random URL-safe base64 chars}` — 192 bits of entropy. `sc_test_` prefix for testnet. Raw key shown once at creation and never stored.

---

### Table: credit_transactions

Append-only ledger of every credit movement for website users. Both purchases (credit additions) and compute charges (credit deductions) are recorded here. This is the financial source of truth for the credit billing path.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | |
| `user_id` | UUID | FK users(id), NOT NULL | |
| `sandbox_id` | UUID | FK sandboxes(id), nullable | Set for compute-charge transactions; NULL for purchases |
| `api_key_id` | UUID | FK api_keys(id), nullable | Which API key triggered this charge; NULL for purchases |
| `type` | TEXT | NOT NULL | purchase, compute_charge, refund, adjustment |
| `amount_usd_cents` | BIGINT | NOT NULL | Always positive. Sign determined by `type`. |
| `balance_after_usd_cents` | BIGINT | NOT NULL | Snapshot of user balance after this transaction (for auditability) |
| `description` | TEXT | NOT NULL | Human-readable description (e.g., nano sandbox - 10s interval, Credit purchase via Stripe) |
| `stripe_payment_intent_id` | TEXT | nullable | Stripe payment intent ID for purchase transactions |
| `interval_start` | TIMESTAMPTZ | nullable | For compute_charge: start of the billed interval |
| `interval_end` | TIMESTAMPTZ | nullable | For compute_charge: end of the billed interval |
| `elapsed_seconds` | NUMERIC(8,3) | nullable | Actual elapsed seconds for compute_charge rows |
| `tier` | TEXT | nullable | Tier being billed for compute_charge rows |
| `partial` | BOOLEAN | NOT NULL, DEFAULT false | True if interval cut short by pause/terminate |
| `created_at` | TIMESTAMPTZ | NOT NULL, DEFAULT now() | |

**Partitioning:** Range-partitioned by month on `created_at`. Old partitions archived after 12 months.

**Indexes:**
- `credit_tx_user_created_idx` ON (user_id, created_at DESC) — billing history queries
- `credit_tx_sandbox_created_idx` ON (sandbox_id, created_at DESC) — per-sandbox billing
- `credit_tx_type_idx` ON type — purchase vs charge filtering

---

### Table: sandboxes

One new nullable column added compared to the L402-only design:

| New Column | Type | Constraints | Description |
|---|---|---|---|
| `api_key_id` | UUID | FK api_keys(id), nullable | Set when sandbox created via API key; NULL for L402 sessions |
| `user_id` | UUID | FK users(id), nullable | Set for API key sessions; NULL for L402 sessions |
| `billing_method` | TEXT | NOT NULL, DEFAULT lightning | lightning or credits — determines which billing engine path handles this sandbox |

All other sandbox columns remain as defined in the base schema (id, agent_id, tier, runtime, status, template, host_vm_id, guest_ip, vsock_cid, wss_token, etc.).

**The `sats_consumed` column is only meaningful for `billing_method = lightning` sandboxes. A new `credits_consumed_usd_cents` column is added for `billing_method = credits` sandboxes:**

| New Column | Type | Constraints | Description |
|---|---|---|---|
| `credits_consumed_usd_cents` | BIGINT | NOT NULL, DEFAULT 0 | Total credits deducted for this sandbox (credit billing path only) |

---

### Table: billing_intervals

Retained as-is for Lightning sessions. Not used for credit billing sessions — those write to `credit_transactions` instead.

For clarity, a `billing_method` column is added:

| New Column | Type | Description |
|---|---|---|
| `billing_method` | TEXT | lightning — all rows in this table are Lightning-billed. Credit-billed intervals are in credit_transactions. |

---

### Table: host_vms

Unchanged from base schema.

---

### Table: templates

Unchanged from base schema.

---

### Table: pending_deposits

Unchanged from base schema (Lightning top-ups for L402 agents only).

---

### Table: audit_log

Two new columns:

| New Column | Type | Description |
|---|---|---|
| `api_key_id` | UUID, nullable | Set when event triggered by an API key request |
| `user_id` | UUID, nullable | Set for website user actions |
| `auth_method` | TEXT, nullable | l402, api_key, or user_jwt |

New event types:
- `api_key.created`
- `api_key.revoked`
- `api_key.auth_failure`
- `credits.purchased`
- `credits.low_balance` — user balance below $1.00
- `credits.out_of_credits` — billing deduction failed; sandbox paused
- `sandbox.paused_no_credits` — sandbox auto-paused due to exhausted credits

---

## Credit Billing Design

### How Credits Work

1. Website user purchases credits via the website dashboard (Stripe payment). Credits are stored in `users.credit_balance_usd_cents`.
2. When an API-key-authenticated agent creates a sandbox, the billing engine spawns a credit billing ticker goroutine (separate from the Lightning ticker).
3. Every 10 seconds, the credit ticker calculates `cost = tier.usd_cents_per_second × elapsed_seconds` and attempts to deduct from the user's balance.
4. Deduction is a single Postgres transaction: check balance >= cost → deduct → insert `credit_transactions` row. The CHECK constraint on `credit_balance_usd_cents >= 0` prevents overdraft at the database level.
5. If deduction fails (balance < cost), the sandbox is paused and the user is notified.

### Credit Deduction Transaction (Atomic)

The deduction must be atomic to prevent race conditions when multiple sandboxes consume credits simultaneously for the same user. The Go billing engine uses a Postgres advisory lock keyed on `user_id` for the duration of each deduction:

```
BEGIN;
SELECT pg_advisory_xact_lock(hashtext(user_id::text));
SELECT credit_balance_usd_cents FROM users WHERE id = user_id FOR UPDATE;
-- check balance >= cost
UPDATE users SET credit_balance_usd_cents = credit_balance_usd_cents - cost WHERE id = user_id;
INSERT INTO credit_transactions (...) VALUES (...);
COMMIT;
```

This ensures that two concurrent 10-second tickers for the same user's two sandboxes do not both read the same balance and both think they have sufficient funds.

### Credit Exhaustion Handling

| Consecutive Failures | Action |
|---|---|
| 1 | `credits.low_balance` warning emitted if balance < $1.00; WebSocket budget_warning frame sent |
| First deduction failure | Sandbox immediately paused; `credits.out_of_credits` audit event; user notified via Supabase Realtime |

Unlike the Lightning path (which has a 3-strike rule due to transient routing failures), credit deduction failure is deterministic — if the balance is insufficient, there is no point retrying the same tick. The sandbox is paused immediately.

### Credit Purchase Flow (Stripe)

Credit purchases are handled by the website frontend + Stripe. The Go binary provides one webhook endpoint for Stripe payment confirmation:

**POST /internal/webhooks/stripe** — Stripe calls this when a payment intent succeeds.

Processing:
1. Verify Stripe webhook signature using `STRIPE_WEBHOOK_SECRET`
2. Extract `user_id` from payment intent metadata
3. Extract `amount_usd_cents` from payment intent amount
4. In a single transaction: increment `users.credit_balance_usd_cents += amount_usd_cents` and insert `credit_transactions` row with `type = purchase`
5. Emit Supabase Realtime event on `users` table update so the dashboard balance refreshes

This endpoint is internal-only (not exposed via Aperture) and protected by Stripe signature verification.

---

## API Key Management Endpoints

These endpoints are called by the website dashboard. They are authenticated via Supabase Auth JWT (the website user's session token) — not by L402 or API key. All routes are prefixed `/v1/user/` and excluded from Aperture's L402 gate.

### POST /v1/user/api-keys — Create API Key

**Authentication:** Supabase Auth JWT

**Request body:**
- `name` (string, required) — Human label
- `description` (string, optional)
- `expires_at` (ISO8601, optional)
- `allowed_tiers` (array, optional)
- `ip_allowlist` (array of IPs, optional)
- `rate_limit_rpm` (integer, optional)

**Processing:**
1. Verify Supabase Auth JWT, extract `user_id`
2. Look up `users` row — verify user exists and `warning_level != 'suspended'`
3. Check active key count against `users.max_api_keys`
4. Generate raw key: `sc_live_` + 32 random URL-safe base64 chars via `crypto/rand`
5. Compute `key_hash = SHA-256(raw_key)` and `key_prefix = raw_key[:12]`
6. Insert `api_keys` row linked to `user_id`
7. Write `audit_log` event: `api_key.created`
8. Return raw key — **shown once only**

**Response 201:**
```
{
  "id": "uuid",
  "name": "My MCP Server",
  "key": "sc_live_aB3xK9mPqR7vNwYzL2cD5eF8gH1jI4k",
  "key_prefix": "sc_live_aB3x",
  "created_at": "ISO8601",
  "expires_at": null,
  "allowed_tiers": null,
  "warning": "Save this key now. It will not be shown again."
}
```

---

### GET /v1/user/api-keys — List Keys

**Response 200:** Array of key metadata objects (no raw key, no hash).

---

### PATCH /v1/user/api-keys/{id} — Update Key

Allows updating: `name`, `description`, `expires_at`, `allowed_tiers`, `ip_allowlist`, `rate_limit_rpm`.

**Response 200:** Updated key object.

---

### DELETE /v1/user/api-keys/{id} — Revoke Key

1. Verify key belongs to the authenticated user
2. Set `is_active = false`
3. Purge key from in-process LRU cache immediately
4. Write `audit_log` event: `api_key.revoked`

**Response 200:** `{ id, revoked: true, revoked_at }`

---

### GET /v1/user/credits — Get Credit Balance

**Response 200:**
```
{
  "credit_balance_usd_cents": 4250,
  "credit_balance_display": "$42.50",
  "total_purchased_usd_cents": 10000,
  "total_consumed_usd_cents": 5750,
  "warning_level": "ok",
  "active_sandboxes_count": 2,
  "estimated_hours_remaining": 14.3
}
```

`estimated_hours_remaining` is calculated as: `credit_balance / sum(tier.usd_cents_per_second × 3600)` across all running sandboxes for this user.

---

### GET /v1/user/credits/history — Credit Transaction History

**Query parameters:** `page`, `limit`, `type` (filter by purchase/compute_charge/refund), `sandbox_id`, `from`, `to`

**Response 200:**
```
{
  "transactions": [
    {
      "id": "uuid",
      "type": "compute_charge",
      "amount_usd_cents": 5,
      "balance_after_usd_cents": 4250,
      "description": "micro sandbox - 10s interval",
      "sandbox_id": "uuid",
      "tier": "micro",
      "interval_start": "ISO8601",
      "interval_end": "ISO8601",
      "created_at": "ISO8601"
    }
  ],
  "total_pages": 12
}
```

---

## Row Level Security Policies

### agents table

| Policy Name | Operation | Expression |
|---|---|---|
| `agents_select_own` | SELECT | `auth.uid() = jwt_sub` |
| `agents_update_own` | UPDATE | `auth.uid() = jwt_sub` (label and metadata only) |

### users table

| Policy Name | Operation | Expression |
|---|---|---|
| `users_select_own` | SELECT | `auth.uid() = id` |
| `users_update_own` | UPDATE | `auth.uid() = id` (label and metadata only — balance updates only via service role) |

### api_keys table

| Policy Name | Operation | Expression |
|---|---|---|
| `api_keys_select_own` | SELECT | `user_id = auth.uid()` (key_hash column excluded) |
| `api_keys_insert_own` | INSERT | `user_id = auth.uid()` |
| `api_keys_update_own` | UPDATE | `user_id = auth.uid()` |
| `api_keys_delete_own` | DELETE | `user_id = auth.uid()` |

### credit_transactions table

| Policy Name | Operation | Expression |
|---|---|---|
| `credit_tx_select_own` | SELECT | `user_id = auth.uid()` |

No INSERT/UPDATE/DELETE policies for external clients — only the Go service role writes to this table.

### sandboxes table

| Policy Name | Operation | Expression |
|---|---|---|
| `sandboxes_select_l402` | SELECT | `agent_id IN (SELECT id FROM agents WHERE jwt_sub = auth.uid())` |
| `sandboxes_select_apikey` | SELECT | `user_id = auth.uid()` |

Both policies are OR-combined so a client can see their sandboxes regardless of how they were created.

### billing_intervals table

| Policy Name | Operation | Expression |
|---|---|---|
| `billing_select_own` | SELECT | `agent_id IN (SELECT id FROM agents WHERE jwt_sub = auth.uid())` |

### pending_deposits table

| Policy Name | Operation | Expression |
|---|---|---|
| `deposits_select_own` | SELECT | `agent_id IN (SELECT id FROM agents WHERE jwt_sub = auth.uid())` |

---

## Supabase Auth Configuration

### Two User Types

**Type 1: Lightning Agents (L402 path)**
- Created programmatically by the Go binary using the Supabase Admin API
- Synthetic email, no password, no OAuth
- `app_metadata.offer_type` set

**Type 2: Website Users (API key path)**
- Sign up on the website using Supabase Auth natively (email/password or OAuth: Google, GitHub)
- Supabase handles registration, email verification, and session management
- On first API call to `/v1/user/*`, the Go binary looks up or creates the corresponding `users` row

### JWT Claims Comparison

| Claim | Lightning Agent | Website User |
|---|---|---|
| `sub` | Agent UUID | User UUID (matches `users.id`) |
| `aud` | authenticated | authenticated |
| `exp` | now + 7 days | now + 1 hour (auto-refreshed by Supabase client) |
| `app_metadata.offer_type` | bolt12 / lnurl / lightning_address | not set |
| `app_metadata.is_website_user` | not set | true |

---

## Supabase Realtime

| Table | Events | Use Case |
|---|---|---|
| `billing_intervals` | INSERT | Real-time Lightning billing feed |
| `credit_transactions` | INSERT | Real-time credit deduction feed to dashboard |
| `sandboxes` | UPDATE | Status and warning_level changes |
| `agents` | UPDATE | Lightning balance changes |
| `users` | UPDATE | Credit balance changes, warning_level changes |
| `api_keys` | UPDATE | Revocation events visible to open dashboard sessions |

---

## Connection Pooling

Unchanged. Supavisor transaction mode at port 6543. pgx pool: MaxConns 20, PreferSimpleProtocol true.

The credit deduction transaction uses `pg_advisory_xact_lock` which is compatible with transaction-mode pooling (the lock is released at transaction end, and transaction mode guarantees a single connection for the duration of a transaction).

---

## Migration Strategy

| # | File | Description |
|---|---|---|
| 001 | `create_agents.sql` | L402 agent identities |
| 002 | `create_users.sql` | **New** — website user accounts with credit balance |
| 003 | `create_sandboxes.sql` | Sandboxes with billing_method, user_id, credits_consumed columns |
| 004 | `create_billing_intervals.sql` | Lightning billing log |
| 005 | `create_billing_partitions.sql` | Monthly partitions |
| 006 | `create_host_vms.sql` | VM host pool |
| 007 | `create_templates.sql` | Sandbox templates |
| 008 | `create_tier_pricing.sql` | **New** — pricing table for both billing paths |
| 009 | `create_pending_deposits.sql` | Lightning top-ups |
| 010 | `create_api_keys.sql` | **New** — API keys linked to users |
| 011 | `create_credit_transactions.sql` | **New** — credit ledger |
| 012 | `create_credit_partitions.sql` | **New** — monthly partitions for credit_transactions |
| 013 | `create_audit_log.sql` | Audit log with auth_method columns |
| 014 | `enable_rls.sql` | Enable RLS on all tables |
| 015 | `create_rls_policies.sql` | All RLS policies including new users/api_keys/credit_transactions |
| 016 | `enable_realtime.sql` | Enable realtime on all monitored tables |
| 017 | `seed_templates.sql` | Seed tier_pricing and templates |

---

## Environment Variables

| Variable | Description | Source |
|---|---|---|
| `SUPABASE_URL` | Project URL | Secret Manager |
| `SUPABASE_SERVICE_KEY` | Service role key | Secret Manager |
| `SUPABASE_JWT_SECRET` | JWT validation secret | Secret Manager |
| `SUPABASE_DB_HOST` | Supavisor transaction mode hostname | Secret Manager |
| `SUPABASE_DB_PORT` | 6543 | Env var |
| `SUPABASE_DB_NAME` | postgres | Env var |
| `SUPABASE_DB_USER` | postgres.{project_ref} | Secret Manager |
| `SUPABASE_DB_PASSWORD` | Database password | Secret Manager |
| `STRIPE_WEBHOOK_SECRET` | Stripe webhook signing secret | Secret Manager |
| `API_KEY_CACHE_TTL_SECS` | LRU cache TTL (default: 60) | Env var |
| `API_KEY_CACHE_SIZE` | LRU cache max entries (default: 10000) | Env var |
| `API_KEY_MAX_PER_USER` | Default max keys per user (default: 10) | Env var |
| `LOW_CREDIT_THRESHOLD_CENTS` | Warning threshold (default: 100 = $1.00) | Env var |

---

## Monitoring

| Metric | Type | Alert Threshold |
|---|---|---|
| `db_query_duration_ms` | Histogram by operation | Alert P99 > 200ms |
| `db_pool_acquired` | Gauge | Alert if == MaxConns |
| `db_connection_errors_total` | Counter | Alert if > 10/min |
| `db_credit_deduction_failures_total` | Counter | Monitor — indicates users running out of credits |
| `db_credit_lock_wait_ms` | Histogram | Alert P99 > 100ms (advisory lock contention) |
| `api_key_cache_hits_total` | Counter | Monitor cache efficiency |
| `api_key_cache_misses_total` | Counter | Monitor cache miss rate |
| `api_key_auth_failures_total` | Counter | Alert if > 50/min |
| `db_realtime_subscribers` | Gauge | Monitor concurrent subscribers |
