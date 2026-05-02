# Component 6: Cloud SQL Postgres + Supabase Auth

## v2 changelog

- Renamed: this component is now **"Cloud SQL Postgres + Supabase Auth"**, not "Supabase Backend". The hot-path database is Cloud SQL for PostgreSQL (regional, in-VPC, private IP). Supabase is retained only as the Auth provider for website users (JWT issuance + session management).
- All hot-path tables move to Cloud SQL: `agents`, `users`, `sandboxes`, `host_vms`, `templates`, `tier_pricing`, `api_keys`, `billing_intervals`, `credit_transactions`, `pending_deposits`, `audit_log`, `top_up_intervals` (new), `instance_leases` (new), `rate_limit_buckets` (new).
- Supabase Realtime, Supabase Storage, and Supabase RLS are no longer used. We use Postgres `LISTEN/NOTIFY` + our own WebSocket fan-out for live events. RLS is meaningless because the control plane uses the service role for all writes; client-side reads against Cloud SQL are not exposed.
- New columns on `agents`: `balance_msats`, `balance_low_water_msats` (per-agent override of the engine default).
- New table `top_up_intervals` records every BOLT12 top-up settlement (analogous to the old `billing_intervals` rows but at top-up granularity).
- New table `instance_leases` lets control-plane instances heartbeat their membership when memberlist gossip is unavailable (Cloud SQL fallback for cluster discovery).
- Connection model uses pgx with per-instance pool of 25 connections; no Supavisor pooler in front.

---

## Overview

Cloud SQL for PostgreSQL is the single source of truth for every persistent piece of state in Sockt. It runs as a regional HA instance with a private IP in the `sc-control` subnet, reached over Private Services Access from the control-plane MIG. The control plane uses the `pgx` driver natively (no proxy, no pooler).

Supabase remains as a separate managed service for website-user authentication. We accept Supabase-issued JWTs (verified locally with the JWKS / JWT secret), but no other Supabase service is used.

---

## Services Used

| Service | Role |
|---|---|
| **Cloud SQL for PostgreSQL** | All persistent state — entities, billing, host pool, audit, leases |
| **Supabase Auth** (external) | JWT issuance for website users (email/password, OAuth) |

The following are NOT used:

- Supabase Postgres / Storage / Realtime / RLS / Edge Functions / Database webhooks
- Supavisor pooler (the in-VPC Cloud SQL connection is direct from pgx)
- Cloud SQL Auth Proxy (private IP makes it unnecessary)

---

## Billing Model Split

Two billing paths share schema but diverge in how settlement is recorded:

| Auth Method | Billing Method | Proof of Settlement | Local Counter |
|---|---|---|---|
| **L402 + BOLT12** (Lightning agents) | Prepaid msat balance, threshold-triggered BOLT12 top-up | Lightning preimage in `top_up_intervals.preimage` | `agents.balance_msats` |
| **API Key** (website users) | Prepaid USD-cent balance with periodic per-sandbox allocation | Allocation ledger in `credit_transactions(type='allocation')` | `users.credit_balance_usd_cents` |

Both write per-tick consumption rollups to `billing_intervals` (Lightning) or `credit_transactions` (credits) via batched 30s flushes. There is no Lightning involvement on credit sandboxes.

---

## Database Schema (Cloud SQL)

### Table: `agents`

Stores one row per agent identity registered via the L402 path (Lightning agents).

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | Internal agent identifier |
| `jwt_sub` | UUID | UNIQUE, NOT NULL | Supabase Auth user ID (or self-issued) |
| `offer_type` | TEXT | NOT NULL | `bolt12` or `manual_bolt11` |
| `bolt12_offer` | TEXT | UNIQUE, nullable | BOLT12 offer string |
| `lightning_address` | TEXT | UNIQUE, nullable | For BOLT11 manual mode (display only) |
| `balance_msats` | BIGINT | NOT NULL DEFAULT 0, CHECK >= -1000000 | Prepaid balance; small negative tolerated for in-flight pause |
| `balance_low_water_msats` | BIGINT | nullable | Per-agent override of `BALANCE_LOW_WATER_SECS` * msats_per_second |
| `total_msats_topped_up` | BIGINT | NOT NULL DEFAULT 0 | Lifetime top-ups |
| `total_msats_consumed` | BIGINT | NOT NULL DEFAULT 0 | Lifetime consumption |
| `label` | TEXT | nullable | Agent-assigned label |
| `metadata` | JSONB | nullable | Arbitrary key-value |
| `created_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `last_active_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `warning_level` | TEXT | NOT NULL DEFAULT 'ok' | `ok`, `low_balance`, `out_of_balance`, `suspended` |

**Indexes:**
- `agents_jwt_sub_idx` ON jwt_sub
- `agents_bolt12_offer_idx` ON bolt12_offer

---

### Table: `users`

Stores one row per website user (signed up via Supabase Auth).

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK | Matches Supabase Auth user UUID |
| `email` | TEXT | UNIQUE, NOT NULL | |
| `credit_balance_usd_cents` | BIGINT | NOT NULL DEFAULT 0, CHECK >= 0 | Master credit balance |
| `total_credits_purchased_usd_cents` | BIGINT | NOT NULL DEFAULT 0 | Lifetime purchases |
| `total_credits_consumed_usd_cents` | BIGINT | NOT NULL DEFAULT 0 | Lifetime consumption |
| `plan` | TEXT | NOT NULL DEFAULT 'free' | `free`, `starter`, `pro`, `enterprise` |
| `max_concurrent_sandboxes` | INT | NOT NULL DEFAULT 5 | |
| `max_api_keys` | INT | NOT NULL DEFAULT 10 | |
| `created_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `last_active_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `warning_level` | TEXT | NOT NULL DEFAULT 'ok' | `ok`, `low_credits`, `out_of_credits`, `suspended` |

**Indexes:**
- `users_email_idx` ON email

---

### Table: `tier_pricing`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `tier` | TEXT | PK | `nano`, `micro`, `standard`, `gpu_small`, `gpu_large` |
| `msats_per_second` | BIGINT | NOT NULL | Lightning rate (msats for sub-sat precision) |
| `usd_cents_per_second_x100` | BIGINT | NOT NULL | Credit rate at 1/100-of-a-cent precision |
| `cold_start_msats` | BIGINT | NOT NULL DEFAULT 0 | One-time provisioning charge (Lightning) |
| `cold_start_usd_cents` | INT | NOT NULL DEFAULT 0 | One-time provisioning charge (credits) |
| `updated_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |

The unit changes: `msats_per_second` (was `sats_per_second`) gives the engine the precision it needs to compute fractional-msat values without rounding losses on 1s ticks.

**Example pricing:**

| Tier | msats/sec | usd_cents/sec ×100 | USD/hour |
|---|---|---|---|
| nano | 500 | 14 | ~$0.50 |
| micro | 2000 | 56 | ~$2.00 |
| standard | 8000 | 222 | ~$8.00 |
| gpu_small | 80_000 | 2222 | ~$80.00 |
| gpu_large | 400_000 | 11_111 | ~$400.00 |

---

### Table: `api_keys`

Unchanged in spec; relocated to Cloud SQL.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | |
| `user_id` | UUID | FK users(id), NOT NULL | |
| `key_hash` | TEXT | UNIQUE, NOT NULL | SHA-256 of raw key |
| `key_prefix` | TEXT | NOT NULL | First 12 chars |
| `name` | TEXT | NOT NULL | |
| `created_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `last_used_at` | TIMESTAMPTZ | nullable | |
| `expires_at` | TIMESTAMPTZ | nullable | |
| `is_active` | BOOLEAN | NOT NULL DEFAULT true | |
| `allowed_tiers` | TEXT[] | nullable | |
| `rate_limit_rpm` | INT | nullable | |
| `ip_allowlist` | INET[] | nullable | |
| `description` | TEXT | nullable | |

**Indexes:**
- `api_keys_key_hash_idx` ON key_hash
- `api_keys_user_id_idx` ON user_id

**Cross-instance revocation:** `DELETE` (soft) on this table issues `NOTIFY apikey_revoked, '<api_key_id>'` so all control-plane instances evict the cached key sub-second.

---

### Table: `credit_transactions`

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | |
| `user_id` | UUID | FK users(id), NOT NULL | |
| `sandbox_id` | UUID | FK sandboxes(id), nullable | |
| `api_key_id` | UUID | FK api_keys(id), nullable | |
| `type` | TEXT | NOT NULL | `purchase`, `allocation`, `compute_charge`, `refund`, `adjustment` |
| `amount_usd_cents` | BIGINT | NOT NULL | Always positive; sign determined by `type` |
| `balance_after_usd_cents` | BIGINT | NOT NULL | Balance snapshot |
| `description` | TEXT | NOT NULL | |
| `stripe_payment_intent_id` | TEXT | nullable | Set for purchase |
| `interval_start` | TIMESTAMPTZ | nullable | For `compute_charge` |
| `interval_end` | TIMESTAMPTZ | nullable | For `compute_charge` |
| `elapsed_seconds_x1000` | INT | nullable | Sub-second precision for `compute_charge` |
| `tier` | TEXT | nullable | For `compute_charge` |
| `partial` | BOOLEAN | NOT NULL DEFAULT false | |
| `created_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |

**Partitioning:** monthly by `created_at`, archived after 12 months.

**Indexes:**
- `credit_tx_user_created_idx` ON (user_id, created_at DESC)
- `credit_tx_sandbox_created_idx` ON (sandbox_id, created_at DESC)
- `credit_tx_type_idx` ON type

The new `type='allocation'` rows record per-sandbox 60s prepaid allocations. `type='compute_charge'` rows are the per-30s rollups of metered consumption (these are batched).

---

### Table: `sandboxes`

Schema matches v1 base plus:

| Column | Type | Constraints | Description |
|---|---|---|---|
| `agent_id` | UUID | nullable, FK agents(id) | Set for L402 sandboxes |
| `user_id` | UUID | nullable, FK users(id) | Set for API-key sandboxes |
| `api_key_id` | UUID | nullable, FK api_keys(id) | Set for API-key sandboxes |
| `billing_method` | TEXT | NOT NULL DEFAULT 'lightning' | `lightning` or `credits` |
| `host_vm_id` | TEXT | nullable | Currently-running host |
| `vsock_cid` | INT | nullable | Firecracker only |
| `local_snapshot_path` | TEXT | nullable | `/var/lib/sc/snapshots/{id}/` (host-local SSD) |
| `snapshot_node_host_id` | TEXT | nullable | Which host has the hot snapshot |
| `gcs_snapshot_path` | TEXT | nullable | `gs://sc-snapshots/...` (cold tier) |
| `gcs_uploaded_at` | TIMESTAMPTZ | nullable | When the host-agent finished GCS upload |
| `owner_instance_id` | TEXT | nullable | Last known owning control-plane instance (advisory; ring is the source of truth) |
| `consumed_msats` | BIGINT | NOT NULL DEFAULT 0 | Cumulative Lightning consumption |
| `credits_consumed_usd_cents` | BIGINT | NOT NULL DEFAULT 0 | Cumulative credit consumption |
| `data_jwt_revoked_at` | TIMESTAMPTZ | nullable | If non-null, host-agents reject old data_jwts (defense in depth) |
| `ssh_private_key_wrapped` | BYTEA | nullable | KMS-wrapped DEK + ciphertext for operator backdoor SSH |
| ... | ... | ... | (id, tier, runtime, status, template, created_at, terminated_at, etc. as in v1) |

---

### Table: `billing_intervals`

For Lightning sandboxes only. Per-tick rollup rows (batched 30s flush from the engine).

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | |
| `sandbox_id` | UUID | FK sandboxes(id), NOT NULL | |
| `agent_id` | UUID | FK agents(id), NOT NULL | |
| `interval_start` | TIMESTAMPTZ | NOT NULL | |
| `interval_end` | TIMESTAMPTZ | NOT NULL | |
| `msats_consumed` | BIGINT | NOT NULL | Per-tick consumption |
| `tick_count` | INT | NOT NULL | Number of 1s ticks aggregated into this row |
| `created_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |

**Partitioning:** monthly.

(Per-tick `bolt12_preimage` and `bolt12_invoice` are removed — those live on `top_up_intervals` instead since payment now happens at top-up granularity, not tick granularity.)

---

### Table: `top_up_intervals` (new)

Records every BOLT12 top-up settlement.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | UUID | PK, DEFAULT gen_random_uuid() | |
| `agent_id` | UUID | FK agents(id), NOT NULL | |
| `sandbox_id` | UUID | FK sandboxes(id), nullable | The sandbox that triggered the top-up; NULL for manual top-ups |
| `idempotency_key` | TEXT | NOT NULL UNIQUE | Caller-supplied key |
| `amount_msats` | BIGINT | NOT NULL | |
| `payment_hash` | BYTEA | NOT NULL | |
| `preimage` | BYTEA | nullable | NULL until settled |
| `bolt12_invoice` | TEXT | nullable | The invoice the agent's Lexe returned |
| `status` | TEXT | NOT NULL DEFAULT 'pending' | `pending`, `settled`, `failed` |
| `error_class` | TEXT | nullable | `routing_failure`, `offer_unreachable`, ... |
| `requested_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `settled_at` | TIMESTAMPTZ | nullable | |

**Indexes:**
- `top_up_agent_requested_idx` ON (agent_id, requested_at DESC)
- `top_up_idempotency_idx` ON idempotency_key
- `top_up_payment_hash_idx` ON payment_hash

---

### Table: `pending_deposits`

Unchanged in spec (Lightning manual deposits for BOLT11 fallback agents).

---

### Table: `host_vms`

Unchanged in spec.

---

### Table: `templates`

Unchanged in spec.

---

### Table: `audit_log`

Adds an `instance_id` column to track which control-plane instance produced the event.

| New Column | Type | Description |
|---|---|---|
| `api_key_id` | UUID, nullable | Set when triggered by an API key request |
| `user_id` | UUID, nullable | Set for website-user actions |
| `agent_id` | UUID, nullable | Set for Lightning agent actions |
| `auth_method` | TEXT, nullable | `l402`, `api_key`, `user_jwt`, `data_jwt` |
| `instance_id` | TEXT, nullable | Control-plane instance name (debugging) |

New event types:

- `api_key.created`, `api_key.revoked`, `api_key.auth_failure`
- `credits.purchased`, `credits.low_balance`, `credits.out_of_credits`
- `sandbox.paused_no_credits`, `sandbox.paused_no_balance`
- `billing.lightning.permanent_failure`, `billing.lightning.recovered`
- `cluster.member_joined`, `cluster.member_left`, `cluster.ring_rebalanced`
- `host.unreachable`, `host.drained`

---

### Table: `instance_leases` (new)

Used as the cluster-membership fallback when memberlist gossip is unavailable, and as the canonical source of truth for orphan-sandbox detection.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `instance_id` | TEXT | PK | GCE instance name |
| `private_ip` | INET | NOT NULL | For peer gRPC |
| `started_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `last_heartbeat_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `version` | TEXT | NOT NULL | Binary git SHA |

Each control-plane instance UPSERTs its row every 5s. A row whose `last_heartbeat_at` is older than 30s is considered dead; the cluster module rebalances the ring.

---

### Table: `rate_limit_buckets` (new)

For cross-instance per-principal rate-limit coordination. The token-bucket state for each principal lives in-memory on the instance handling the most recent request, and is sampled to this table every 5s. On request, an instance reads the row to verify the principal's recent global usage. Eventual consistency is acceptable at the 300-rpm thresholds in use.

| Column | Type | Constraints | Description |
|---|---|---|---|
| `principal_key` | TEXT | PK | `agent_id` or `api_key_id` (hashed) |
| `tokens_remaining` | INT | NOT NULL | Current bucket level |
| `bucket_size` | INT | NOT NULL | Per-second cap |
| `last_refilled_at` | TIMESTAMPTZ | NOT NULL DEFAULT now() | |
| `updated_by_instance_id` | TEXT | NOT NULL | Last instance that wrote |

(For tighter rate limiting, a Redis or per-host token bucket can be introduced later — out of scope for v1.)

---

## Lightning Path: Settlement Flow Recap

Per the new billing engine (Component 3):

1. The billing engine meters in-memory; no DB writes per tick
2. Every 30s the engine bulk-inserts `billing_intervals` rows (batched per-tick rollups)
3. When a sandbox's `balance_msats` falls below threshold, the engine calls `payment.TopUpBalance` which inserts a `top_up_intervals` row in `pending` status, then updates it to `settled` with `preimage` on success
4. On settle, `agents.balance_msats` is incremented; in-memory cache is updated
5. `audit_log` rows track non-routine events (top-up failures, pauses, etc.)

The constraint `agents.balance_msats >= -1000000` (allows up to 1 sat negative) tolerates the 1-tick window between balance reaching zero and the orchestrator's `PauseSandbox` returning.

---

## Credits Path: Allocation Flow Recap

1. The billing engine maintains a per-sandbox in-memory `BalanceCents` (the local prepaid allocation)
2. When that balance drops below 30s of runtime, the engine calls `AllocateCreditsForSandbox` which:
   ```
   BEGIN;
   SELECT pg_advisory_xact_lock(hashtext(user_id::text));
   SELECT credit_balance_usd_cents FROM users WHERE id = $1 FOR UPDATE;
   IF balance >= requested → UPDATE users SET credit_balance -= requested; INSERT credit_transactions(type='allocation', ...)
   ELSE IF balance > 0 → allocate balance, set to 0
   ELSE → return ErrInsufficient
   COMMIT;
   ```
3. Every 30s the engine bulk-inserts `credit_transactions(type='compute_charge', ...)` rows for actual consumption
4. On reconciliation: unused allocations (sandbox terminated before consumption) are refunded via `type='refund'` rows

---

## API Key Management Endpoints

These endpoints are called by the website dashboard and authenticated via Supabase Auth JWT.

### POST /v1/user/api-keys — Create API Key
### GET /v1/user/api-keys — List Keys
### PATCH /v1/user/api-keys/{id} — Update Key
### DELETE /v1/user/api-keys/{id} — Revoke Key
### GET /v1/user/credits — Get Credit Balance
### GET /v1/user/credits/history — Credit Transaction History

Behavior unchanged from v1 in shape. Implementation now talks to Cloud SQL directly (no Supabase JS client).

The `DELETE` endpoint additionally issues `NOTIFY apikey_revoked, '<id>'` so all control-plane instances evict their LRU caches sub-second.

---

## Auth: Supabase or Self-Issued

We have two compatible options for issuing JWTs:

1. **Supabase Auth (recommended for v1)** — leverages the existing email/password + OAuth flows, email verification, password reset, magic links. The control plane verifies the JWT signature against Supabase's published JWKS or shared `SUPABASE_JWT_SECRET`.
2. **Self-issued JWTs** — drop the Supabase dependency entirely and run our own auth (e.g. via `github.com/markbates/goth` for OAuth). More implementation work; useful if Supabase Auth becomes a bottleneck or limitation.

The interface is the same either way: a JWT with `sub = users.id` is presented in `Authorization: Bearer <jwt>` and the control plane's `JWTMiddleware` validates it. Lightning agents (L402 path) use a separately self-signed JWT issued by the platform at offer registration time.

### JWT Claims

| Claim | Lightning Agent | Website User |
|---|---|---|
| `sub` | Agent UUID | User UUID (matches `users.id`) |
| `aud` | `sockt` | `authenticated` |
| `exp` | now + 7 days | now + 1 hour (auto-refreshed by Supabase client) |
| `app_metadata.offer_type` | bolt12 / manual_bolt11 | not set |
| `app_metadata.is_website_user` | not set | true |

---

## Realtime / Pub-Sub Replacement

In place of Supabase Realtime, we use Cloud SQL `LISTEN/NOTIFY` for cross-instance event distribution and our own WebSocket fan-out (in the host-agent for billing events; in the control plane for dashboard events).

| Channel | When | Consumed By |
|---|---|---|
| `apikey_revoked` | DELETE on `api_keys` | All control-plane instances (LRU cache eviction) |
| `agent_balance_change` | INSERT on `top_up_intervals` (settled) | Dashboard WS connections |
| `user_warning_level` | UPDATE on `users.warning_level` | Dashboard WS connections |
| `host_status_change` | UPDATE on `host_vms.status` | All control-plane instances (host pool refresh) |
| `tier_pricing_changed` | UPDATE on `tier_pricing` | All control-plane instances (in-memory price cache) |

LISTEN connections are held in dedicated goroutines per instance (one per channel) with auto-reconnect on connection drop.

For dashboard live updates, the control plane exposes `GET /v1/user/events` (HTTP/2 streaming response). The handler subscribes to the relevant LISTEN channels and forwards filtered events to the connected dashboard.

---

## Connection Pooling

`pgx` per-instance pool:

| Setting | Value |
|---|---|
| `MaxConns` | 25 |
| `MinConns` | 5 |
| `MaxConnLifetime` | 30 min |
| `MaxConnIdleTime` | 5 min |
| `HealthCheckPeriod` | 1 min |

The credit deduction transaction uses `pg_advisory_xact_lock` directly against this pool (no transaction-mode pooler in between, so locks behave exactly as documented).

For a 30-instance MIG: `30 × 25 = 750` max connections, well within Cloud SQL's `db-custom-8-32768` ceiling of `200 × 8 = 1600` connections (configurable up to ~3000). Read replica adds capacity for heavy reads.

---

## Migration Strategy

Migrations are managed by `dbmate` against Cloud SQL.

| # | File | Description |
|---|---|---|
| 001 | `create_agents.sql` | Lightning agent identities + balance columns |
| 002 | `create_users.sql` | Website user accounts with credit balance |
| 003 | `create_sandboxes.sql` | Sandboxes with billing_method, host/snapshot/owner columns |
| 004 | `create_billing_intervals.sql` | Lightning per-tick rollup |
| 005 | `create_billing_partitions.sql` | Monthly partitions |
| 006 | `create_top_up_intervals.sql` | New: Lightning top-up settlements |
| 007 | `create_host_vms.sql` | VM host pool |
| 008 | `create_templates.sql` | Sandbox templates |
| 009 | `create_tier_pricing.sql` | Pricing in msats and cents-x100 |
| 010 | `create_pending_deposits.sql` | Lightning manual deposits |
| 011 | `create_api_keys.sql` | API keys |
| 012 | `create_credit_transactions.sql` | Credit ledger (purchase, allocation, charge, refund) |
| 013 | `create_credit_partitions.sql` | Monthly partitions |
| 014 | `create_audit_log.sql` | Audit log with auth_method + instance_id |
| 015 | `create_instance_leases.sql` | New: control-plane membership |
| 016 | `create_rate_limit_buckets.sql` | New: cross-instance rate limits |
| 017 | `seed_templates.sql` | Seed tier_pricing and templates |
| 018 | `enable_listen_notify_triggers.sql` | Triggers that fire NOTIFY on relevant updates |

(There is no `enable_rls.sql` or `create_rls_policies.sql`. RLS is unused.)

---

## Environment Variables

| Variable | Description | Source |
|---|---|---|
| `CLOUDSQL_HOST` | Cloud SQL private IP (`10.4.0.3`) | Env var |
| `CLOUDSQL_PORT` | `5432` | Env var |
| `CLOUDSQL_DB` | `sc` | Env var |
| `CLOUDSQL_USER` | `control_plane` | Env var |
| `CLOUDSQL_PASSWORD` | DB password | Secret Manager |
| `CLOUDSQL_RO_HOST` | Read replica private IP | Env var |
| `SUPABASE_JWT_SECRET` | JWT verify secret (verify only) | Secret Manager |
| `SUPABASE_AUTH_URL` | For dashboard redirect (no server-side use) | Env var |
| `STRIPE_WEBHOOK_SECRET` | Stripe signing secret | Secret Manager |
| `API_KEY_CACHE_SIZE` | LRU max entries (default: 10000) | Env var |
| `LOW_CREDIT_THRESHOLD_CENTS` | Default: 100 ($1.00) | Env var |
| `LOW_BALANCE_THRESHOLD_MSATS` | Default: tier-dependent (set per tier) | Env var |

---

## Monitoring

| Metric | Type | Alert Threshold |
|---|---|---|
| `db_query_duration_ms` | Histogram by op | Alert P99 > 100ms |
| `db_pool_acquired` | Gauge | Alert if == MaxConns for > 60s |
| `db_pool_acquire_wait_ms` | Histogram | Alert P99 > 50ms |
| `db_connection_errors_total` | Counter | Alert if > 10/min |
| `db_credit_allocation_failures_total` | Counter | Monitor — users running out of credits |
| `db_credit_lock_wait_ms` | Histogram | Alert P99 > 100ms |
| `db_listen_notify_received_total` | Counter | by channel |
| `db_listen_notify_disconnects_total` | Counter | Alert if any non-zero (channel reconnect issue) |
| `cloudsql_cpu_pct` | Gauge (Cloud Monitoring) | Alert > 80% for 10 min |
| `cloudsql_replication_lag_ms` | Gauge | Alert > 5000 ms |

---

## Dependencies

- **Cloud SQL for PostgreSQL** (Component 9) — primary data store
- **Supabase Auth** — JWT issuance for website users
- **GCP Secret Manager** for DB and JWT secrets
- **Control Plane Binary** — every other component depends on this schema
