# Component 6: Supabase Backend (MVP)

## v3 changelog

- Cloud SQL removed entirely from MVP.
- Supabase Postgres is now the only database for all persistent state.
- Supabase Auth remains for website users.
- Supabase Realtime is now the default event fan-out layer.
- `instance_leases` and `rate_limit_buckets` tables removed from MVP schema.
- Pooling strategy split between Supavisor transaction and session modes.

---

## Overview

For MVP, Supabase is the single backend service for:

- Postgres data
- user auth
- realtime event delivery
- light object storage needs

This removes the fixed baseline cost and operational complexity of Cloud SQL HA.

---

## Services Used

| Service | Role |
|---|---|
| Supabase Postgres | Primary relational datastore |
| Supabase Auth | Dashboard user auth |
| Supabase Realtime | Low-latency event fan-out |
| Supabase Storage | Small artifacts (optional backups, metadata files) |

---

## Connection Strategy

### Primary pool

- Supavisor transaction mode (`:6543`)
- Used by short, request-scoped DB operations
- Good fit for Cloud Run stateless handlers

### Session-affinity pool

- Supavisor session mode (`:5432`)
- Used only where connection affinity is required:
  - `pg_advisory_xact_lock` credit allocation path
  - long-lived consumers when needed

### Direct connection

- Limited usage for admin/migrations only
- Avoid for normal traffic on free tier

---

## Billing Model Support

Same two billing methods are preserved:

| Auth Method | Billing Method | Counter |
|---|---|---|
| L402 | Prepaid msat balance | `agents.balance_msats` |
| API key | Prepaid credits | `users.credit_balance_usd_cents` |

Key difference from v2: metering happens in runtime pods and audit updates are pushed back in batches.

---

## Core MVP Schema

Keep these tables:

- `agents`
- `users`
- `api_keys`
- `sandboxes`
- `tier_pricing`
- `billing_intervals`
- `credit_transactions`
- `top_up_intervals`
- `pending_deposits`
- `audit_log`
- `templates`

Removed in MVP:

- `instance_leases` (no cluster membership)
- `rate_limit_buckets` (no multi-instance strong coordination requirement)

---

## Events (Realtime-first)

Use Supabase Realtime channels for MVP updates:

| Channel | Event |
|---|---|
| `apikey_revoked` | invalidate API key cache |
| `agent_balance_change` | top-up or balance warning |
| `user_warning_level` | low/out-of-credits UI updates |
| `sandbox_state` | running/paused/terminated transitions |

`LISTEN/NOTIFY` can remain a Phase 2 enhancement if event volume grows.

---

## Partitioning and Retention

MVP simplification:

- no monthly partitioning initially
- use plain tables with indexes
- introduce partitions only when DB size approaches ~400 MB

Retention policy:

- keep detailed intervals for 30 days
- aggregate and archive older records

---

## Migration Plan (MVP)

Migration set stays mostly compatible with v2, with removals:

- keep table-creation migrations for core entities
- keep pricing/template seed migration
- remove migrations for `instance_leases` and `rate_limit_buckets`
- remove RLS-related migrations for control-plane service flows

---

## Env Vars

| Variable | Description |
|---|---|
| `SUPABASE_URL` | project base URL |
| `SUPABASE_SERVICE_ROLE_KEY` | server-side access key |
| `SUPABASE_DB_TX_DSN` | transaction pool DSN (`:6543`) |
| `SUPABASE_DB_SESSION_DSN` | session pool DSN (`:5432`) |
| `SUPABASE_JWT_SECRET` | JWT verification |
| `LOW_CREDIT_THRESHOLD_CENTS` | low-credit warning threshold |

---

## Performance and Limits Notes

- Free tier DB size target: 500 MB hard cap
- Keep indexes minimal and focused
- Batch inserts for billing records every 30s
- Keep concurrent DB client count low via pool tuning

Recommended pool defaults:

- `MaxConns=15`
- `MinConns=2`
- `MaxConnLifetime=30m`
- `MaxConnIdleTime=5m`

---

## Monitoring

Track:

- query latency p95/p99
- pool acquisition wait
- failed DB auth/connections
- realtime channel delivery failures
- table growth (`billing_intervals`, `credit_transactions`, `audit_log`)

---

## Dependencies

- Control plane API (Component 2)
- Billing engine callbacks (Component 3)
- Payment module (Component 5)
- Orchestrator (Component 4)

---

## Phase 2 (post-MVP)

Restore v2 data architecture if scale requires:

- Cloud SQL private HA primary + replica
- `instance_leases` cluster fallback table
- `rate_limit_buckets` cross-instance limiter table
- partition-heavy schema for high write volume
