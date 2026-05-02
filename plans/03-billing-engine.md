# Component 3: Billing Engine

## v2 changelog

- Per-tick BOLT12 pull-pay replaced by a **prepaid msat balance** model. Every active sandbox has an in-memory `balance_msats` counter. Metering happens every 1 second in-memory (no DB, no Lightning, no I/O on the critical path).
- Lightning settlement is **decoupled** from metering. When `balance_msats < topup_threshold` the engine triggers a single BOLT12 pull payment for `topup_chunk_sats` (default: 5 minutes of runtime). On 3 consecutive top-up failures, the sandbox is paused.
- Audit ledger writes (`billing_intervals` for L402, `credit_transactions` for credits) are **batched** every 30 seconds. One bulk INSERT covers all sandboxes' completed intervals.
- The 1-goroutine-per-sandbox ticker is collapsed into a **single global ticker + worker pool** (default: 64 workers). 10k sandboxes are billed by 64 goroutines, not 10k.
- Ticker ownership is now **sharded by consistent hash** of `sandbox_id` across the control-plane MIG instances. Each instance only meters the sandboxes it owns. Recovery on startup hydrates only owned sandboxes.
- Credit (USD-cent) path uses the same model: in-memory cents counter per user, threshold-triggered Stripe top-up is *not* automatic (users top up manually via dashboard), but the per-sandbox in-memory cents counter is decremented locally and the user's balance is consulted only when the per-sandbox local "credit allowance" is exhausted.
- The 3-strike rule for Lightning failures is replaced by a 3-strike rule for **top-up** failures (which are more meaningful — every metering tick succeeds in-memory).

---

## Overview

The Billing Engine is the `internal/billing` package within the **control-plane binary**. It is the financial core of the platform.

The engine handles two billing paths determined by `sandbox.billing_method` at creation time:

- **Lightning Path** (`billing_method = lightning`) — for L402-authenticated agents. Maintains an in-memory `balance_msats` per sandbox; meters at sub-second granularity; pulls top-ups from the agent's BOLT12 offer when the balance drops below threshold.
- **Credits Path** (`billing_method = credits`) — for API-key-authenticated website users. Maintains an in-memory cents counter per sandbox; periodically reconciles to `users.credit_balance_usd_cents`; pauses the sandbox when the user's overall balance hits zero.

Both paths share: the global ticker, the worker pool, the in-memory balance cache, the batched audit-flush mechanism, the WSS billing fan-out, and the consistent-hash ownership scheme.

---

## Core Concepts

### In-Memory Balance Cache

The engine maintains an in-memory map (`internal/billing/balance.go`) keyed by `sandbox_id`:

```go
type SandboxBalance struct {
    SandboxID         uuid.UUID
    BillingMethod     string // "lightning" | "credits"
    OwnerKey          string // agent_id or user_id
    Tier              string
    MsatsPerSecond    int64
    BalanceMsats      atomic.Int64
    ConsumedMsats     atomic.Int64    // lifetime
    LastMeteredAt     atomic.Pointer[time.Time]
    PendingAuditRows  []BillingInterval // protected by sync.Mutex; flushed every 30s
    TopUpInFlight     atomic.Bool
    ConsecutiveTopUpFailures atomic.Int32
    LowBalanceWarned  atomic.Bool
}
```

For the credits path, `MsatsPerSecond` is replaced by `CentsPerSecond` and `BalanceMsats` becomes `BalanceCents`. The cache is generic over the unit but the rest of the model is identical.

### Single Global Ticker + Worker Pool

Instead of one goroutine per sandbox, the engine runs:

- **One global ticker** at 1s interval (configurable via `BILLING_TICK_MS`)
- **A buffered channel** of `*SandboxBalance` (capacity: 16384)
- **N workers** (default: 64, configurable) reading from the channel and metering one sandbox each

Per tick: the ticker walks the in-memory map and pushes each sandbox onto the channel. Workers pop, measure elapsed seconds since `LastMeteredAt`, decrement the balance, append a pending audit row, and check the top-up threshold. Memory is ~80MB total for the cache (10k sandboxes × ~8KB each), vs. 80MB+200MB ticker overhead in the per-goroutine model.

### Decoupled Settlement

Metering is purely in-memory and never blocks on Lightning, the database, or any network call. Settlement happens asynchronously:

- **Top-up settlement** — when threshold reached, the worker submits a top-up job to a separate top-up goroutine pool (small, 8 workers); meter continues unimpeded
- **Audit settlement** — the audit-flush goroutine wakes every 30s and batch-inserts all pending audit rows in a single SQL statement

If a top-up is in flight, the meter still ticks down the balance. If the balance reaches zero before the top-up returns, the sandbox is marked `pause_pending` but the meter keeps ticking (just into negative balance) until the orchestrator's pause RPC completes — the user is not under-charged. Audit rows are flushed regardless of top-up success.

### Consistent-Hash Ownership

Each control-plane instance asks `cluster.OwnerOf(sandbox_id)` before adding a sandbox to its in-memory cache. Only the owning instance meters that sandbox. On ring rebalance:
- Sandboxes that move to this instance: hydrated from `agents.balance_msats` / `users.credit_balance_usd_cents` and added to the cache
- Sandboxes that move away: pending audit rows flushed immediately, balance written back to Cloud SQL, removed from the cache

This makes the in-process billing state correct under horizontal autoscaling.

---

## Module Structure

### BillingEngine

Top-level struct managing the in-memory cache and the goroutines.

Fields:
- `Store` interface
- `Payment` interface (used by top-up goroutines)
- `Orchestrator` interface (used to call `PauseSandbox` on top-up exhaustion)
- `HostAgentClients` map (used to push `UpdateBalance` to host-agents for WSS fan-out)
- `Cluster` interface (for ownership decisions)
- `cache map[uuid.UUID]*SandboxBalance` (sync.RWMutex)
- `tickerWorkPool chan *SandboxBalance` (buffered)
- `topupPool chan TopUpJob` (buffered)
- `auditFlushTicker *time.Ticker` (30s)
- `quit chan struct{}` (graceful shutdown)

### Workers

| Pool | Count | Job |
|---|---|---|
| `meterWorkers` | 64 (configurable) | Per-tick metering for one sandbox |
| `topupWorkers` | 8 | Execute one BOLT12 top-up payment |
| `auditFlushWorker` | 1 | Drain pending audit rows every 30s and bulk-insert |
| `balanceWriteBackWorker` | 1 | Write balance snapshots to Cloud SQL every 60s for crash recovery |
| `tickerLoop` | 1 | The global 1s tick loop |
| `ringChangeListener` | 1 | Reacts to cluster ring rebalances; triggers cache hydration/eviction |

---

## Lightning Billing Path (billing_method = lightning)

### Tick Step

```
Worker receives *SandboxBalance from tickerWorkPool
│
├─ now := time.Now()
├─ last := bal.LastMeteredAt.Load()
├─ elapsed_secs := (now - *last).Seconds()
├─ msats_owed := round(bal.MsatsPerSecond * elapsed_secs)
│
├─ new_balance := bal.BalanceMsats.Add(-msats_owed)
├─ bal.ConsumedMsats.Add(msats_owed)
├─ bal.LastMeteredAt.Store(&now)
│
├─ append BillingInterval{start: *last, end: now, msats: msats_owed} to bal.PendingAuditRows
│
├─ push BalanceUpdate to host-agent (best effort, async — used for WSS fan-out)
│
├─ if new_balance < topup_threshold_msats AND !bal.TopUpInFlight.Swap(true):
│       enqueue TopUpJob{sandbox_id, agent_id, chunk_sats}
│
└─ if new_balance < 0 AND !bal.PausePending.Swap(true):
       enqueue PauseJob{sandbox_id, reason: "balance_exhausted"}
```

Per-tick cost: ~100ns of CPU, no I/O.

### Top-Up Step

A top-up worker dequeues a `TopUpJob` and:

```
preimage, err := payment.TopUpBalance(ctx, agent_id, chunk_sats, idempotency_key=tick_id)
│
├─ if err == nil:
│       bal.BalanceMsats.Add(chunk_sats * 1000)
│       bal.ConsecutiveTopUpFailures.Store(0)
│       bal.TopUpInFlight.Store(false)
│       store.RecordTopUp(...)
│       host_agent.UpdateBalance(...) // push to host so WSS can broadcast a top-up event
│
├─ else (transient: routing failure, offer unreachable, timeout):
│       fails := bal.ConsecutiveTopUpFailures.Add(1)
│       bal.TopUpInFlight.Store(false)
│       host_agent.NotifyTopUpFailure(sandbox_id, fails)
│       if fails >= 3:
│           orchestrator.PauseSandbox(sandbox_id) // takes the sandbox out of metering
│
└─ else (permanent: offer_invalid, payment_rejected):
       bal.TopUpInFlight.Store(false)
       orchestrator.PauseSandbox(sandbox_id)
       audit_log: billing.lightning.permanent_failure
```

Top-up timeout is 8 seconds; the meter does NOT block on this.

### WebSocket Billing Frame Injection

On every metering tick, the engine pushes a `BalanceUpdate` to the host-agent that owns this sandbox via gRPC. The host-agent's billing fan-out (Component 8) sends the update to any open WSS shell sessions:

```json
{
  "type": "billing",
  "billing_method": "lightning",
  "interval_msats": 20000,
  "balance_msats_remaining": 4_320_000,
  "consumed_msats_total": 6_840_000,
  "metered_at": "ISO8601"
}
```

The push is async and best-effort: a slow/disconnected host-agent does not block metering. If the host-agent is briefly unreachable, the next successful push overwrites with the latest cumulative state (idempotent).

### Top-Up Settlement Frame

When a top-up settles successfully, an additional WSS frame is sent:

```json
{
  "type": "topup",
  "amount_sats": 600,
  "amount_msats": 600_000,
  "balance_msats_after": 4_920_000,
  "preimage": "abc123...",
  "settled_at": "ISO8601"
}
```

---

## Credits Billing Path (billing_method = credits)

### Tick Step

Same as Lightning, except:
- Unit is `cents` (or fractional cents tracked at sub-cent precision by accumulating the residual)
- No external Lightning settlement — instead, the per-sandbox `BalanceCents` cache is replenished from the user's `credit_balance_usd_cents` when needed

The per-sandbox cache is allocated in chunks (default: 60s of runtime) to avoid hammering the user's balance row on every tick. When a sandbox's local cache drops below 30s of runtime:

```
Worker enqueues a CreditAllocationJob{user_id, sandbox_id, requested_cents}
│
└─ Single Postgres transaction (Cloud SQL):
   BEGIN;
   SELECT pg_advisory_xact_lock(hashtext(user_id::text));
   SELECT credit_balance_usd_cents FROM users WHERE id = user_id FOR UPDATE;
   IF balance >= requested → balance -= requested → INSERT credit_transactions(type='allocation', ...)
   ELSE IF balance > 0 → allocate balance, balance := 0, INSERT credit_transactions
   ELSE → return ErrInsufficient
   COMMIT;
```

The allocation job is enqueued to the same `topupPool` used by Lightning top-ups. Workers in the pool dispatch to the right backend based on the job's `billing_method`.

This shape — local cache replenished from a global balance — collapses cross-instance contention on `users.credit_balance_usd_cents` from "every tick × every sandbox" to "every 60s × every sandbox owned by the user *on this instance*". For a user with 10 sandboxes, the user-balance row is updated about 10 times per minute regardless of how many control-plane instances they're spread across.

### Out-of-Credits Handling

If `CreditAllocationJob` returns `ErrInsufficient`:
1. The sandbox's local `BalanceCents` reaches zero on the next tick
2. `PauseJob{reason: "out_of_credits"}` enqueued
3. Orchestrator pauses the sandbox
4. Audit row inserted: `credit_transactions.type = 'compute_charge'` for the partial interval, then a final `audit_log.event = 'credits.out_of_credits'`
5. `users.warning_level` updated to `out_of_credits`; we publish an event on `NOTIFY user_warning_level` so any open dashboard websockets refresh

### Low Balance Warning

When the user's overall balance falls below `LOW_CREDIT_THRESHOLD_CENTS` (default 100 cents = $1.00), a single warning is emitted:
- Per sandbox WSS frame (from any one sandbox owned by this user)
- Cloud SQL `NOTIFY user_warning_level` for dashboard refresh

`lowBalanceWarned` is set on the user (not per sandbox) to avoid duplicate warnings.

---

## WebSocket Billing Frames by Path

### Lightning billing frame
```json
{
  "type": "billing",
  "billing_method": "lightning",
  "interval_msats": 2_000,
  "balance_msats_remaining": 4_320_000,
  "consumed_msats_total": 6_840_000,
  "metered_at": "ISO8601"
}
```

### Lightning top-up frame
```json
{
  "type": "topup",
  "amount_sats": 600,
  "amount_msats": 600_000,
  "balance_msats_after": 4_920_000,
  "preimage": "abc...",
  "settled_at": "ISO8601"
}
```

### Lightning low-balance warning
```json
{
  "type": "low_balance",
  "balance_msats": 8_000,
  "estimated_seconds_remaining": 4,
  "next_topup_attempt_at": "ISO8601"
}
```

### Lightning out-of-balance frame
```json
{
  "type": "out_of_balance",
  "consecutive_topup_failures": 3,
  "last_failure_reason": "no_route",
  "paused_at": "ISO8601"
}
```

### Credits billing frame
```json
{
  "type": "billing",
  "billing_method": "credits",
  "interval_usd_cents_x100": 28,
  "interval_display": "$0.0028",
  "balance_remaining_usd_cents": 4000,
  "balance_remaining_display": "$40.00",
  "metered_at": "ISO8601"
}
```

(`interval_usd_cents_x100` keeps two extra digits of precision for sub-cent ticks; clients can divide by 100 to render dollars.)

### Credits low/out frames

Identical in shape to v1 (`low_credits`, `out_of_credits`).

---

## BillingEngine Operations

### RegisterSandbox

Called by the `api` module after a sandbox reaches `running` status (and after the initial top-up has settled).

1. If `cluster.IsOwner(sandbox_id) == false`: forward the registration RPC to the owning instance
2. Build `SandboxBalance` with the initial top-up amount as `BalanceMsats` (Lightning) or initial cents allocation (credits)
3. Insert into the in-memory cache
4. Set `LastMeteredAt = now()`
5. Push the initial balance to the host-agent so WSS clients can render it on connect

### DeregisterSandbox

Called when a sandbox transitions to `paused` or `terminated`.

1. Compute final partial-interval cost
2. Append to pending audit rows; force a synchronous flush for this sandbox's rows
3. Write back the final `balance_msats` to Cloud SQL `agents.balance_msats` (keep the unspent prepaid funds with the agent)
4. Push final WSS frame
5. Remove from in-memory cache

### MigrateOwnership

Called by the cluster ring callback. Receives `(added []sandbox_id, removed []sandbox_id)`.

For each `removed`: behave like `DeregisterSandbox` (without pausing) — flush, write back, evict.
For each `added`: hydrate from Cloud SQL (read `sandboxes.balance_msats` snapshot, plus tier from `tier_pricing`) and insert into the cache.

Because the meter advances `LastMeteredAt` only on its own ticks, an in-flight migration window of < 5 seconds creates at most a single duplicate or missing tick. Both are detectable in the nightly reconciliation job and trivially correctable.

### RecoverTickers (startup)

On binary startup:
1. Wait for `cluster.LocalInstanceID()` to be assigned and the ring to stabilize (default: 5s)
2. Query Cloud SQL: `SELECT id FROM sandboxes WHERE status = 'running'`
3. For each, check `cluster.IsOwner(id)`; if true, hydrate and register; if false, skip (the owner will hydrate it on its own startup or via ring callback)

### ForceSettle (reconciliation job)

Used by the nightly Cloud Scheduler reconciliation:
- For Lightning: find any sandboxes whose in-memory `consumed_msats` differs from `SUM(billing_intervals.msats)` for the day → flag for review
- For credits: find any `credit_transactions` with `type='allocation'` whose total exceeds the corresponding sandbox's actual elapsed runtime × tier rate → refund the excess

---

## Concurrent Credit Allocation

The 60s-allocation-chunk model already serializes most contention: a user with 10 sandboxes triggers ~10 allocation transactions per minute, not 10 per second. The Postgres advisory lock (`pg_advisory_xact_lock(hashtext(user_id))`) serializes those few allocations cleanly with sub-millisecond hold time.

If allocation contention ever becomes a hotspot, the next step is to coalesce: a single allocation job per user per instance per cycle, sized to cover all that user's locally-owned sandboxes for the next 60s. This is a future optimization; not needed for v1.

---

## Reconciliation Job

A nightly Cloud Scheduler job runs `cmd/reconcile/main.go`:

### Lightning path
- For each Lightning sandbox: verify `agents.balance_msats + SUM(billing_intervals.msats since reset)` ≈ `SUM(top_up_intervals.amount_msats)`. Discrepancy > 1000 msats → flag.
- Find any `top_up_intervals` with `status='pending'` older than 1h → retry once or mark `failed`.

### Credits path
- Find `credit_transactions` rows of type `allocation` whose paired `compute_charge` rows sum to less than the allocation (sandbox terminated before consuming all of it) → record a `credit_transactions.type='refund'` row for the unused portion and add it back to `users.credit_balance_usd_cents`.
- Find users with `warning_level='out_of_credits'` and `credit_balance_usd_cents > 0` → push a `NOTIFY user_warning_level` event so the dashboard prompts a resume.

---

## Store Interface Additions

For the prepaid model:

- `IncrementAgentBalance(ctx, agentID, deltaMsats) → (newBalance int64, err error)`
- `DecrementAgentBalance(ctx, agentID, deltaMsats) → (newBalance int64, err error)`
- `BatchInsertBillingIntervals(ctx, []BillingInterval) → error` — bulk insert via `INSERT ... SELECT FROM unnest(...)`
- `RecordTopUp(ctx, TopUp) → error`
- `GetTopUpHistory(ctx, agentID, page, limit) → []TopUp`

For credits:

- `AllocateCreditsForSandbox(ctx, userID, sandboxID, apiKeyID, requestedCents) → (allocatedCents int64, err error)` — performs the SELECT FOR UPDATE, decrement, and `credit_transactions(type='allocation')` insert in one transaction
- `BatchInsertCreditCharges(ctx, []CreditCharge) → error` — periodic per-tick rollup writes
- `RefundUnusedAllocation(ctx, userID, sandboxID, refundCents) → error`

---

## Performance Characteristics

### Per-tick cost (in-memory only)
- ~100 ns CPU per sandbox tick
- 10k sandboxes × 1 tick/sec / 64 workers = ~156 ticks/worker/sec → ~16µs per worker per tick → workers are 99% idle

### Audit flush load
- Pending rows accumulated for 30s = 30 intervals/sandbox × 10k sandboxes = 300k rows per flush
- Bulk INSERT via `unnest` arrays: ~150ms per flush against Cloud SQL — acceptable

### Top-up Lightning load
- One BOLT12 payment per sandbox per ~5 minutes (default `BALANCE_TOPUP_CHUNK_SECS = 300`, `BALANCE_LOW_WATER_SECS = 60`)
- 10k sandboxes / 5 min = ~33 payments/sec — well within LND HA pair capacity (multi-channel routing supports >100/sec/node)
- Down ~30x from the v1 1000 payments/sec at 10s ticks

### Credit allocation load
- One allocation per sandbox per 60s = 167 transactions/sec at 10k sandboxes — well within Cloud SQL capacity
- Per-user advisory lock contention is bounded by sandboxes-per-user / 60s → almost never queues

---

## Monitoring & Alerting

### Metrics

| Metric | Type | Labels | Alert |
|---|---|---|---|
| `billing_active_sandboxes` | Gauge | billing_method | Alert if drops > 10% in 1 min |
| `billing_meter_tick_duration_us` | Histogram | — | Alert P99 > 1ms (should be sub-µs) |
| `billing_meter_workers_busy` | Gauge | — | Alert if = pool size for > 60s (worker starvation) |
| `billing_topup_in_flight` | Gauge | — | Monitor |
| `billing_topup_duration_ms` | Histogram | result | Alert P99 > 8000ms |
| `billing_topup_failures_total` | Counter | reason (no_route, offer_unreachable, timeout, ...) | Alert if rate > 50/min |
| `billing_topup_settlement_total` | Counter | — | Operational health |
| `billing_audit_flush_duration_ms` | Histogram | — | Alert P99 > 2000ms |
| `billing_audit_flush_rows` | Histogram | — | Monitor batch size |
| `billing_audit_pending_rows` | Gauge | — | Alert if > 1M (back-pressure) |
| `billing_credit_allocations_total` | Counter | — | Operational |
| `billing_credit_allocation_failures_total` | Counter | reason | Alert if `insufficient_balance` > 10/min |
| `billing_balance_zero_pauses_total` | Counter | billing_method | Alert if rate > 20/min |
| `billing_ring_migration_events_total` | Counter | direction | Operational |

### Log Events

| Event | Level | When |
|---|---|---|
| `billing.engine.started` | INFO | Startup, includes `BILLING_TICK_MS` and `BILLING_AUDIT_FLUSH_SECS` |
| `billing.sandbox.registered` | INFO | New sandbox added to in-memory cache |
| `billing.sandbox.deregistered` | INFO | Removed (pause/terminate) |
| `billing.lightning.topup_settled` | DEBUG | BOLT12 top-up succeeded |
| `billing.lightning.topup_failed` | WARN | Top-up failed (with reason) |
| `billing.lightning.topup_exhausted` | ERROR | 3 consecutive failures → pause |
| `billing.lightning.permanent_failure` | ERROR | offer_invalid / payment_rejected → pause |
| `billing.credits.allocated` | DEBUG | Allocation succeeded |
| `billing.credits.allocation_failed` | WARN | Insufficient balance |
| `billing.credits.exhausted` | ERROR | Local cache hit zero with no allocation refill → pause |
| `billing.audit.flush_completed` | DEBUG | Periodic flush summary (rows, duration) |
| `billing.recovery.started` | INFO | Startup ticker recovery for owned sandboxes |
| `billing.ring.migrated` | INFO | Ring rebalance: N added, M removed |

---

## Dependencies

- **Payment Module** (Component 5) — `TopUpBalance` (Lightning path)
- **Store Module** — Cloud SQL reads/writes for both paths
- **Orchestrator** (Component 4) — `PauseSandbox` on top-up exhaustion
- **Cluster Module** (Component 2 / `internal/cluster`) — sandbox ownership decisions
- **Host-Agent** (Component 2 / `cmd/host-agent`) — receives `UpdateBalance` gRPC pushes for WSS fan-out
- **Cloud SQL** — `LISTEN/NOTIFY` for warning-level fan-out to dashboards
