# Component 3: Billing Engine

## Overview

The Billing Engine is the `internal/billing` module within the unified Go binary. It is the financial core of the platform, responsible for metering every active sandbox and settling payment on a 10-second tick cycle.

The engine handles two completely separate billing paths determined by `sandbox.billing_method` at creation time:

- **Lightning Path** (`billing_method = lightning`) — for L402-authenticated agents. Uses BOLT12 pull-pay: every 10 seconds, requests a BOLT12 invoice from the agent's registered offer, pays it via LND, stores the preimage in `billing_intervals`.
- **Credits Path** (`billing_method = credits`) — for API-key-authenticated website users. Uses synchronous Postgres deduction: every 10 seconds, atomically deducts USD cents from `users.credit_balance_usd_cents` and inserts a row into `credit_transactions`. No Lightning involved.

Both paths share the same goroutine model, tick interval, failure handling philosophy, and WebSocket event injection mechanism. Only the payment settlement step differs.

---

## Core Concepts

### Billing Ticker

Each active sandbox has exactly one billing ticker goroutine. The ticker is spawned when a sandbox transitions to `running` and is cancelled when the sandbox transitions to `paused` or `terminated`.

The ticker runs on a configurable interval (default: **10 seconds**). At each tick it:

1. Measures the exact elapsed seconds since the last tick
2. Calculates cost for the interval (sats or USD cents depending on billing_method)
3. Calls the appropriate settlement function (Lightning or Credits)
4. Records the result in the appropriate ledger table
5. Increments the sandbox's cumulative cost counter
6. Emits a billing event to any connected WebSocket clients

### Billing Path Selection

The ticker reads `sandbox.billing_method` at spawn time. This field is set by the API handler when the sandbox is created based on the auth method of the creating request:

| Auth Method at Creation | billing_method | Ticker Type |
|---|---|---|
| L402 + macaroon | lightning | LightningTicker |
| API key (Bearer sc_live_...) | credits | CreditsTicker |

The billing_method never changes for the lifetime of a sandbox. A sandbox created via API key always uses credit billing, even if the same user later authenticates via L402.

### Billing Precision

Cost per tick is calculated with full precision before rounding:

- **Lightning:** `sats_owed = round(tier.sats_per_second × actual_elapsed_seconds, 0)` — rounded to nearest sat
- **Credits:** `cost_usd_cents = round(tier.usd_cents_per_second × actual_elapsed_seconds, 0)` — rounded to nearest cent (minimum 1 cent)

`actual_elapsed_seconds` is wall-clock measured, not the scheduled interval, preventing billing drift on slow ticks.

---

## Module Structure

### BillingEngine

Top-level struct managing the full collection of active tickers. Holds:
- `Store` interface — for all DB operations
- `Payment` interface — for Lightning operations (Lightning path only)
- `Orchestrator` interface — for pausing sandboxes on billing failure
- `sync.Map` of `sandbox_id → Ticker` — unified ticker registry for both paths
- Channel for graceful shutdown signal

### LightningTicker

Handles the Lightning billing path. Fields:
- Sandbox reference (tier, agent_id, bolt12_offer)
- `context.Context` with cancel
- `consecutiveFailures` int — reset to 0 on success; Lightning failures can be transient
- `lastTickAt` timestamp

### CreditsTicker

Handles the credit billing path. Fields:
- Sandbox reference (tier, user_id)
- `context.Context` with cancel
- `lastTickAt` timestamp
- No `consecutiveFailures` — credit failures are deterministic (insufficient balance = pause immediately)
- `lowBalanceWarned` bool — tracks whether the low-balance warning has been sent this session

---

## Lightning Billing Path (billing_method = lightning)

### Happy Path Per Tick

```
Ticker wakes at T+10s
│
├─ elapsed = now() - lastTickAt
├─ sats_owed = round(tier.sats_per_second × elapsed, 0)
│
├─ payment.RequestBOLT12Invoice(agent.bolt12_offer, sats_owed, memo)
│   └─ LND sends invoice_request to agent's Lexe node via onion message
│   └─ Lexe node returns signed BOLT12 invoice
│
├─ payment.PayInvoice(bolt12_invoice)
│   └─ LND routes payment; agent's Lexe node settles HTLC
│   └─ Returns preimage (proof of payment)
│
├─ store.RecordBillingInterval(sandbox_id, agent_id, start, end,
│     sats_owed, bolt12_invoice, preimage, tick_number)
│
├─ store.IncrementSandboxSats(sandbox_id, sats_owed)
│
├─ Emit WebSocket billing frame
└─ lastTickAt = now(); consecutiveFailures = 0
```

### Lightning Failure Handling

Lightning payment failures can be transient (no routing path, temporary node unreachability). The engine applies a 3-strike rule:

| consecutiveFailures | Action |
|---|---|
| 1 | Emit `budget_warning` WebSocket frame (level: warning). Record failed interval row. |
| 2 | Emit `budget_warning` WebSocket frame (level: critical). |
| 3 | Call `orchestrator.PauseSandbox`. Stop ticker. Write `audit_log: sandbox.paused_billing_failure`. |

On each failed interval, a `billing_intervals` row is written with `status = failed` and `bolt12_preimage = NULL`.

---

## Credits Billing Path (billing_method = credits)

### Happy Path Per Tick

```
Ticker wakes at T+10s
│
├─ elapsed = now() - lastTickAt
├─ cost_cents = round(tier.usd_cents_per_second × elapsed, 0)
│   (minimum 1 cent per tick)
│
├─ store.DeductCredits(user_id, sandbox_id, api_key_id, cost_cents,
│     tier, start, end, elapsed)
│   │
│   └─ Single Postgres transaction:
│       BEGIN;
│       SELECT pg_advisory_xact_lock(hashtext(user_id));
│       SELECT credit_balance_usd_cents FROM users WHERE id=user_id FOR UPDATE;
│       IF balance < cost_cents → ROLLBACK → return ErrInsufficientCredits
│       UPDATE users SET credit_balance_usd_cents -= cost_cents,
│                        total_credits_consumed_usd_cents += cost_cents;
│       INSERT INTO credit_transactions (user_id, sandbox_id, api_key_id,
│           type='compute_charge', amount_usd_cents=cost_cents,
│           balance_after_usd_cents, description, tier,
│           interval_start, interval_end, elapsed_seconds);
│       COMMIT;
│
├─ store.IncrementSandboxCredits(sandbox_id, cost_cents)
│
├─ Check remaining balance → emit low_balance warning if < LOW_CREDIT_THRESHOLD_CENTS
│
├─ Emit WebSocket billing frame (type: credits)
└─ lastTickAt = now()
```

### Credits Failure Handling

Credit deduction failure is deterministic — if the balance is insufficient, retrying next tick will not help (balance only decreases). The sandbox is paused immediately on the first deduction failure.

```
DeductCredits returns ErrInsufficientCredits
│
├─ Emit WebSocket frame: { type: "out_of_credits", balance_usd_cents: N }
├─ Call orchestrator.PauseSandbox(sandbox_id)
├─ Stop ticker
├─ Write audit_log: credits.out_of_credits + sandbox.paused_no_credits
└─ Update sandboxes.warning_level = 'suspended'
```

The user is then notified via Supabase Realtime (the `users` table update triggers a Realtime event to the dashboard).

### Low Balance Warning

Before the balance is fully exhausted, a proactive warning is emitted. The CreditsTicker checks the remaining balance after each successful deduction:

- If `balance_after < LOW_CREDIT_THRESHOLD_CENTS` (default: 100 cents = $1.00) AND `lowBalanceWarned == false`:
  - Emit WebSocket frame: `{ type: "low_credits", balance_usd_cents: N, estimated_minutes_remaining: M }`
  - Set `lowBalanceWarned = true`
  - Update `users.warning_level = 'low_credits'` in Supabase (triggers Realtime event to dashboard)

`estimated_minutes_remaining` is calculated as: `balance_after / (tier.usd_cents_per_second × 60)`.

---

## WebSocket Billing Frames by Path

### Lightning billing frame

```json
{
  "type": "billing",
  "billing_method": "lightning",
  "interval_sats": 20,
  "total_sats_consumed": 840,
  "preimage": "abc123...",
  "interval_start": "ISO8601",
  "interval_end": "ISO8601"
}
```

### Credits billing frame

```json
{
  "type": "billing",
  "billing_method": "credits",
  "interval_usd_cents": 5,
  "interval_display": "$0.05",
  "total_credits_consumed_usd_cents": 250,
  "balance_remaining_usd_cents": 4000,
  "balance_remaining_display": "$40.00",
  "interval_start": "ISO8601",
  "interval_end": "ISO8601"
}
```

### Low credits warning frame (credits path only)

```json
{
  "type": "low_credits",
  "balance_usd_cents": 85,
  "balance_display": "$0.85",
  "estimated_minutes_remaining": 12.7,
  "message": "Less than $1.00 remaining. Add credits to keep your sandbox running."
}
```

### Out of credits frame (credits path only)

```json
{
  "type": "out_of_credits",
  "balance_usd_cents": 0,
  "message": "Credit balance exhausted. Sandbox has been paused. Add credits and resume.",
  "paused_at": "ISO8601"
}
```

---

## BillingEngine Operations

### StartBilling

Called by the `api` module after a sandbox reaches `running` status.

1. Read `sandbox.billing_method`
2. If `lightning`: look up agent's `bolt12_offer` from Supabase; spawn `LightningTicker` goroutine
3. If `credits`: look up `sandbox.user_id`; spawn `CreditsTicker` goroutine
4. Register ticker in `sync.Map` by `sandbox_id`

### StopBilling

Called when sandbox transitions to `paused` or `terminated`.

1. Retrieve ticker from `sync.Map`
2. Calculate partial interval cost (elapsed since last tick)
3. Settle partial interval:
   - **Lightning:** if `partial_sats >= 1`, attempt BOLT12 payment for partial interval
   - **Credits:** if `partial_cents >= 1`, run `DeductCredits` for partial interval (with `partial = true` in credit_transactions)
4. Cancel context → goroutine exits
5. Remove from `sync.Map`

### RecoverTickers (startup)

On binary startup, query Supabase for all `sandboxes WHERE status = 'running'`. For each, read `billing_method` and call `StartBilling`. This recovers billing after Cloud Run instance restarts.

### ForceSettle

Used by the reconciliation job. Settles any `credit_transactions` or `billing_intervals` rows with `status = 'unsettled'` or `status = 'failed'`.

---

## Concurrent Credit Deduction

Multiple sandboxes owned by the same user tick simultaneously. Without coordination, two tickers could both read the same balance and both proceed, causing an overdraft. The Postgres advisory lock (`pg_advisory_xact_lock(hashtext(user_id))`) serializes deductions for the same user at the database level. The Go tickers do not need any in-process mutex — the database handles it.

**Performance note:** At the default 10-second interval, a single user would need more than 10 concurrent sandboxes before consecutive ticks overlap for the same user. Advisory lock acquisition is fast (< 1ms when uncontested). For users with many concurrent sandboxes, the locks will queue briefly — this is acceptable given the 10-second tick window.

---

## Reconciliation Job

A nightly Cloud Scheduler job runs `cmd/reconcile/main.go`:

### Lightning path reconciliation
- Find `billing_intervals` rows with `status = 'unsettled'`; retry BOLT12 payment up to 3 times
- Find sandboxes with `status = 'running'` and no active ticker; restart ticker or force-pause

### Credits path reconciliation
- Find `credit_transactions` rows with `type = 'compute_charge'` where the corresponding sandbox is `terminated` but the transaction amount seems inconsistent with uptime — flag for manual review
- Find users with `warning_level = 'out_of_credits'` and `credit_balance_usd_cents > 0` (credits topped up since pause) — send Supabase Realtime notification prompting user to resume

---

## Store Interface Additions

New methods added to the `Store` interface to support the credits billing path:

- `DeductCredits(ctx, userID, sandboxID, apiKeyID, costCents, tier, start, end, elapsed, partial) → error`
- `GetUserCreditBalance(ctx, userID) → (balanceCents int64, err error)`
- `IncrementSandboxCredits(ctx, sandboxID, costCents) → error`
- `GetCreditTransactions(ctx, userID, filter) → []CreditTransaction`
- `UpdateUserWarningLevel(ctx, userID, level) → error`
- `GetTierPricing(ctx) → map[string]TierPricing`

---

## Performance Characteristics

### Credits path database load

At 10,000 concurrent credit-billed sandboxes with 10-second intervals:
- 1,000 credit deduction transactions/second
- Each deduction is a 3-statement Postgres transaction (advisory lock + SELECT FOR UPDATE + UPDATE + INSERT)
- Batching is applied: the billing engine collects credit_transactions inserts and performs one batched INSERT per second (1,000 rows at once) rather than 1,000 individual transactions
- The UPDATE to `users.credit_balance_usd_cents` cannot be batched (advisory lock required per user); however, multiple same-user ticks within the same second are coalesced by accumulating the total cost and running one deduction

### Lightning path database load

Unchanged from base spec. Billing intervals batched at 1 INSERT/second.

---

## Monitoring & Alerting

### Metrics

| Metric | Type | Labels | Alert |
|---|---|---|---|
| `billing_tickers_active` | Gauge | billing_method (lightning/credits) | Alert if drops > 10% in 1 min |
| `billing_payment_duration_ms` | Histogram | billing_method | Alert P99 > 8000ms (Lightning only) |
| `billing_failures_total` | Counter | billing_method, reason | Alert if rate > 50/min |
| `billing_credit_deductions_total` | Counter | tier | Monitor spend by tier |
| `billing_credit_deduction_errors_total` | Counter | reason | Alert if insufficient_balance > 10/min |
| `billing_sats_settled_total` | Counter | tier | Lightning revenue tracking |
| `billing_intervals_unsettled` | Gauge | — | Alert if > 100 (Lightning) |
| `billing_credit_lock_wait_ms` | Histogram | — | Alert P99 > 100ms |

### Log Events

| Event | Level | When |
|---|---|---|
| `billing.ticker.started` | INFO | Goroutine spawned (includes billing_method) |
| `billing.ticker.stopped` | INFO | Cancelled (includes reason) |
| `billing.lightning.settled` | DEBUG | Happy path Lightning interval |
| `billing.lightning.failed` | WARN | Lightning payment failed |
| `billing.lightning.suspended` | ERROR | 3rd consecutive failure → pause |
| `billing.credits.deducted` | DEBUG | Happy path credit deduction |
| `billing.credits.low_balance` | WARN | Balance below threshold |
| `billing.credits.exhausted` | ERROR | Insufficient balance → pause |
| `billing.recovery.started` | INFO | Startup ticker recovery |

---

## Dependencies

- **Payment Module** (Component 5) — `RequestBOLT12Invoice`, `PayInvoice` (Lightning path only)
- **Store Module** — all DB reads and writes for both billing paths
- **Orchestrator** (Component 4) — `PauseSandbox` on billing failure (both paths)
- **WebSocket Server** (Component 8) — billing frame injection to connected shell clients
- **Supabase Realtime** — `users` table updates broadcast credit balance changes to dashboard
