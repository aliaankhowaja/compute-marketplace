# Component 3: Billing Engine (Runtime-Local MVP)

## v3 changelog

- Removed centralized global ticker from control plane.
- Moved 1-second metering loop to `internal/runtime/meter.go` in each pod.
- Removed cluster ownership/ring migration logic.
- Control plane billing reduced to callback ingestion + durable writes.
- Top-up trigger path converted to runtime callback -> control plane payment.

---

## Overview

Billing now uses a split model:

- **Runtime pod** performs high-frequency (1s) balance decrement in-memory.
- **Control plane** handles durable persistence and payment actions when callbacks arrive.

This preserves prepaid behavior while allowing Cloud Run to scale to zero.

---

## Core Model

Each running sandbox has local runtime state:

```go
type RuntimeBalance struct {
  SandboxID      string
  BillingMethod  string // lightning or credits
  RatePerSecond  int64
  Balance        int64
  ConsumedTotal  int64
  LastMeteredAt  time.Time
  TopupInFlight  bool
}
```

### Tick loop

- interval: 1 second
- decrement local balance
- append interval to local audit buffer
- emit WSS billing frame
- if below threshold and no top-up in-flight: call control-plane top-up trigger endpoint
- if balance <= 0: call control-plane pause trigger endpoint

---

## Callback Endpoints

Runtime pod posts to control plane:

1. `POST /v1/runtime/topup-trigger`
2. `POST /v1/runtime/pause-trigger`
3. `POST /v1/runtime/billing/audit-batch`

Audit batches are sent every 30 seconds (or on pause/terminate).

---

## Lightning Path

1. Runtime meter decrements `balance_msats`.
2. When threshold crossed, runtime triggers top-up callback.
3. Control plane calls payment module `TopUpBalance`.
4. On success, control plane persists new balance and returns delta.
5. Runtime applies delta and emits top-up event.
6. On repeated failure, runtime requests pause.

No per-tick Lightning operations occur.

---

## Credits Path

1. Runtime meter decrements local cents balance.
2. Threshold callback requests additional credit allocation.
3. Control plane performs DB transaction with advisory lock.
4. Allocated amount is returned to runtime and applied locally.
5. If no credits remain, runtime requests pause.

---

## Audit Persistence

Runtime keeps a local ring buffer of 30-second intervals.

Every 30 seconds:

- push batched intervals to control plane
- control plane bulk inserts into:
  - `billing_intervals` (Lightning)
  - `credit_transactions` (credits)

On pause/terminate:

- runtime flushes remaining buffer before stop when possible

---

## WSS Frames

Generated locally by runtime:

- `billing`
- `topup`
- `low_balance`
- `out_of_balance`

No control-plane `UpdateBalance` push is needed in MVP.

---

## Failure Behavior

| Failure | Handling |
|---|---|
| callback timeout | retry with backoff and jitter |
| control plane unavailable | continue local metering up to safety floor |
| prolonged callback failure | pause sandbox to avoid under-accounting |
| top-up permanent failure | pause sandbox |

Safety floor:

- allow limited negative drift window
- require reconciliation before next resume

---

## Reconciliation

A periodic reconciliation job compares:

- runtime-reported consumption totals
- persisted audit rows
- account balance deltas

Discrepancies beyond threshold are flagged and auto-adjusted when safe.

---

## Monitoring

Runtime metrics:

- `runtime_meter_tick_ms`
- `runtime_balance`
- `runtime_audit_buffer_size`
- `runtime_callback_failures_total`

Control-plane metrics:

- `billing_audit_batch_ingest_ms`
- `billing_topup_trigger_total`
- `billing_pause_trigger_total`
- `billing_reconcile_discrepancies_total`

---

## Dependencies

- Runtime binary (Component 2)
- Payment module (Component 5)
- Supabase backend (Component 6)
- Orchestrator pause API (Component 4)
- WSS protocol (Component 8)

---

## Phase 2 (post-MVP)

If scale requires central coordination, restore v2 approach:

- global centralized meter
- distributed ownership ring
- worker-pool based tick fanout
