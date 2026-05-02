# Component 5: Payment Module

## v2 changelog

- New `TopUpBalance(ctx, agentID, sats, idempotencyKey)` is the primary entry point for the billing engine. It encapsulates the BOLT12 invoice_request → pay flow into a single idempotent operation that funds the agent's prepaid msat balance.
- LND deployed as an **active-passive HA pair** (`lnd-node-primary`, `lnd-node-secondary`). The payment module discovers the active node at startup via the `lnd-active` GCE label and rediscovers on gRPC connection error.
- New **pre-minted BOLT11 invoice pool** (`internal/payment/invoicepool.go`) keeps ~1000 1-sat invoices warm for the L402 middleware. Removes the 50–200ms `lnd.AddInvoice` call from every 402 response.
- All payment operations carry an `idempotency_key`. Lightning payments are idempotent by `payment_hash` (same payment_hash → no duplicate HTLC), and our application-level layer also dedupes by key in case the HTLC failed cleanly and we want to safely retry.
- Per-tick BOLT12 logic removed — Lightning settlement is now triggered only by the billing engine's threshold logic (Component 3) or the manual `POST /v1/sandbox/{id}/extend-balance` endpoint.
- BOLT11 fallback for non-BOLT12 wallets converted to a "manual top-up" mode where the agent pre-deposits sats periodically (no per-tick LNURL polling).

---

## Overview

The Payment Module is the `internal/payment` package within the **control-plane binary**. It abstracts all Lightning Network interactions into a clean interface consumed by the Billing Engine, the API handlers, and the native L402 middleware.

It manages two Lightning implementations: LND (the platform's own HA pair) and the BOLT12 onion messaging layer via LNDK.

The module is the only component in the system that communicates directly with the Lightning Network. All other modules call the `Payment` interface.

---

## Responsibilities

- Maintain a persistent gRPC connection to the active LND node (with automatic failover to the secondary)
- Mint a pool of pre-paid BOLT11 invoices for L402 challenges; refill in background
- Issue BOLT12 invoice requests to agent offers via LNDK onion messaging
- Pay BOLT12 invoices returned by agent Lexe nodes
- Encapsulate the BOLT12 invoice_request → pay → preimage flow as `TopUpBalance`
- Issue BOLT11 deposit invoices for agent top-ups (manual fallback path)
- Validate BOLT12 offer strings before storage
- Report node channel balance and routing health
- Manage payment idempotency at the application layer

---

## Lightning Node Infrastructure

### Platform LND HA Pair

Two LND nodes operating from the **same node identity** via shared `tls.cert`, `tls.key`, `wallet.db`, and `channel.backup`:

- `lnd-node-primary` at `10.0.0.5:10009`
- `lnd-node-secondary` at `10.0.0.6:10009`

At any given time exactly one is **active** (serving gRPC, online to the Lightning network). The other has its `lnd` process stopped to avoid double-signing.

**LND specifications:**
- LND version 0.18+ (full BOLT12 reliability)
- LNDK sidecar for BOLT12 onion messaging
- Bitcoin backend: `bitcoind` (pruned) on the same VM
- Channel policy: auto-managed by autopilot with manual overrides for key routing nodes
- Minimum channel size: 5,000,000 sats
- Total channel capacity target: 100,000,000 sats (1 BTC) across 10+ well-connected routing peers

### Failover Detection (Payment Module Side)

On startup, the payment module:
1. Calls the GCP Compute API to find the instance tagged `lnd-active` in `sc-control` subnet
2. Opens a persistent gRPC channel to that instance's IP
3. Subscribes to LND `GetInfo` updates as a liveness probe (every 5s)

If the gRPC channel returns `Unavailable` or `DeadlineExceeded` for > 10 seconds:
1. Re-query GCP Compute API for the current `lnd-active` label
2. If the label has moved to the other instance, reconnect to the new active
3. If both nodes are unreachable, mark the payment module unhealthy and refuse new top-up requests (existing in-flight payments may still complete via LND idempotency)

A separate failover daemon on each LND VM (Component 9) is responsible for the actual primary→secondary promotion (channel-backup restore + label update + `lnd` start). The payment module is purely a client of whichever node is currently active.

### LNDK Sidecar

LNDK adds BOLT12 onion messaging to LND without replacing it:
- Intercepts onion messages arriving at the active LND node
- Parses BOLT12 `invoice_request` messages and routes to/from the platform
- Returns BOLT12 invoices from agents back to the calling LND client

### Alternative: Core Lightning (CLN)

Should LNDK prove insufficient, the module is designed to swap to CLN with minimal interface changes. The `Payment` interface abstracts away the underlying RPC schema. The L402 middleware would also need to switch (it currently reads from LND for invoice subscriptions).

---

## Payment Interface

### Offer Management

**`ValidateOffer(offer string) → error`**
- Decodes the BOLT12 offer locally; checks structural validity, network prefix
- Pure function; no network I/O

**`CacheOffer(agentID UUID, offer string)`**
- Stores the offer string in an in-memory LRU cache (capacity: 100,000)
- Cache key: `agentID`
- TTL: 1 hour, refreshed on use
- Reduces Cloud SQL reads for the billing engine's frequent agent lookups

### Top-Up (the primary new entry point)

**`TopUpBalance(ctx, agentID UUID, sats int64, idempotencyKey string) → (preimage string, err error)`**

This is the operation the billing engine calls when a sandbox's balance falls below threshold. It is a single idempotent API that:

1. Checks the in-process idempotency cache (`internal/payment/idem.go`) for `idempotencyKey`. On cache hit, returns the cached `(preimage, err)` without re-attempting.
2. Looks up the agent's `bolt12_offer` (or `lightning_address`) — from in-memory LRU first, then Cloud SQL
3. Constructs a BOLT12 `invoice_request` with `amount_msat = sats * 1000` and `payer_note = "topup:<agent_id>:<idempotency_key>"`
4. Sends via LNDK onion messaging
5. Receives the BOLT12 invoice from the agent's Lexe node (timeout: 6s)
6. Pays the invoice via LND `SendPaymentV2` with explicit timeout (default: 25s) and `payment_request` (which inherently dedupes by `payment_hash`)
7. On success: store the result in the idempotency cache (TTL 24h) and return the preimage
8. On failure: classify error and return; the caller will retry with the same `idempotencyKey`

**Idempotency semantics:**
- Same `idempotencyKey` within 24h → returns the cached preimage (or cached error) without making another payment
- The cache is per-instance; if the originating instance restarts, the cache is lost. In that case, LND's own `payment_hash`-based dedup prevents double-payment at the protocol level (the second `SendPaymentV2` with the same `payment_hash` returns the original payment record).

**Failure modes (returned err):**

| Class | Examples | Billing Action |
|---|---|---|
| `routing_failure` | No route, insufficient liquidity | Retry next billing top-up cycle; increment consecutive_failures |
| `offer_unreachable` | Onion message timeout | Retry; increment consecutive_failures |
| `invoice_expired` | Agent's 30s invoice window missed | Retry immediately (regenerate invoice_request); do not increment failures |
| `payment_rejected` | Agent explicitly rejected HTLC | Permanent failure; pause sandbox |
| `offer_invalid` | Offer decodes to invalid structure | Permanent failure; notify ops |
| `lnd_unavailable` | gRPC connection lost; failover in progress | Hold and retry; do not increment failures |
| `amount_below_minimum` | Agent's offer minimum > requested sats | Multiply chunk size next attempt; record event |

### Other Invoice Operations

**`RequestBOLT12Invoice(ctx, offer string, amountSats int64, memo string) → (BOLT12Invoice, error)`**
- Lower-level helper used by `TopUpBalance` internally
- Sends `invoice_request` via LNDK and returns the signed invoice
- Timeout: 8 seconds

**`PayInvoice(ctx, invoice string, idempotencyKey string) → (Preimage, error)`**
- Pays a BOLT11 or BOLT12 invoice via LND `SendPaymentV2`
- Idempotent by `payment_hash` (extracted from the invoice)
- Default timeout: 25 seconds

**`CreateBOLT11Invoice(ctx, amountSats int64, memo string, expirySeconds int) → (BOLT11Invoice, error)`**
- Used by: deposit endpoint, the L402 invoice pool's refill goroutine
- Memo format for L402 pool: `"L402-pool"` (no per-request data; macaroon binds them later)
- Memo format for deposits: `"deposit:{agent_id}:{amount_sats}"`

**`MintL402Invoice() → (BOLT11Invoice, preimage, error)`**
- Pulls from the pre-minted invoice pool
- Returns both the invoice and its preimage so the macaroon's `payment_hash` can be set
- If the pool is empty (rare), falls back to a synchronous `lnd.AddInvoice` call

**`GetInvoiceStatus(ctx, paymentHash string) → InvoiceStatus`**
- Used by deposit polling and reconciliation
- Returns: `pending | settled | expired | cancelled`

### Balance & Health

**`GetNodeBalance(ctx) → BalanceInfo`**
- Returns: `wallet_balance_sats`, `channel_balance_sats`, `pending_open_sats`, `pending_htlc_sats`, `active_channels`, `inactive_channels`

**`GetNodeHealth(ctx) → NodeHealth`**
- Returns: `{lnd_connected, lndk_connected, synced_to_chain, block_height, active_node_label}`
- Used by the control plane's `/healthz`

**`VerifyPreimage(preimage string, paymentHash string) → bool`**
- Stateless `sha256(preimage) == paymentHash`
- Used by the L402 middleware

---

## Pre-Minted BOLT11 Invoice Pool

### Pool Structure

```go
type InvoicePool struct {
    pool     chan *PoolEntry  // buffered, capacity = 2x target size
    target   int              // 1000 by default
    minSats  int64            // 1
    refill   chan struct{}    // signal channel for the refill goroutine
}

type PoolEntry struct {
    BOLT11      string
    PaymentHash []byte
    Preimage    []byte    // we pre-generate so we can hand the L402 middleware both halves
    ExpiresAt   time.Time
}
```

### Refill Logic

A single goroutine (`internal/payment/invoicepool.go`) wakes whenever `len(pool) < target/2`. It:
1. Generates a random 32-byte preimage
2. Computes `payment_hash = sha256(preimage)`
3. Calls LND `AddInvoice` with the explicit `r_preimage` and `value_msat = 1000`
4. Pushes the resulting `PoolEntry` onto the pool

We use the `r_preimage` field of LND's `AddInvoice` so the platform knows the preimage at mint time (without it, the agent would learn the preimage only on payment, defeating the L402 model).

### Eviction

Each entry has a 1-hour expiry. A second goroutine evicts expired entries (rare; the pool turns over within minutes under typical L402 load). Evicted invoices are also `CancelInvoice`d on LND so they don't stick around as pending HTLCs.

### Numbers

At 100 new agents per second hitting L402 cold-start, the pool drains at 100/sec. The refill goroutine sustains 100 mints/sec against LND `AddInvoice` (which can do > 1000/sec sustained). The pool stays full; 402 responses always pull a warm entry. P50 402 latency drops from ~150ms to < 5ms.

---

## LND gRPC Client Configuration

The LND gRPC connection is established at binary startup and re-established on failover.

### Connection Parameters

| Parameter | Value |
|---|---|
| Target | Discovered via `lnd-active` GCE label (`10.0.0.5:10009` or `10.0.0.6:10009`) |
| TLS cert | Mounted from GCP Secret Manager (`lnd-tls-cert`) at startup; shared by both nodes |
| Macaroon | Scoped: `invoices:read`, `invoices:write`, `offchain:read`, `offchain:write` |
| Max recv message size | 16 MB |
| Keepalive interval | 15 seconds |
| Keepalive timeout | 5 seconds |
| Reconnect backoff | Exponential (initial: 1s, max: 30s) |
| Failover trigger | `Unavailable`/`DeadlineExceeded` for > 10s consecutive |

### LND gRPC Services Used

| Service | Methods Used |
|---|---|
| `lnrpc.Lightning` | `GetInfo`, `ChannelBalance`, `WalletBalance`, `AddInvoice`, `LookupInvoice`, `CancelInvoice` |
| `routerrpc.Router` | `SendPaymentV2`, `TrackPaymentV2` |
| `lnrpc.Lightning` (BOLT12 via LNDK) | `SendCustomMessage`, `SubscribeCustomMessages` |
| `invoicesrpc.Invoices` | `SubscribeSingleInvoice` (deposit detection) |

---

## BOLT12 Top-Up Sequence Detail (called by `TopUpBalance`)

### Step 1: invoice_request Construction

The payment module constructs a BOLT12 `invoice_request` with:
- `offer`: agent's stored offer string
- `amount_msat`: `sats * 1000`
- `payer_note`: `"topup:{agent_id}:{idempotency_key}"`
- `payer_id`: platform node's pubkey
- `created_at`: now
- `payer_metadata`: opaque (can include sandbox_id batch info for agent audit)

### Step 2: Send via Onion Message

Encoded as a TLV blob, delivered via LNDK onion messaging to the blinded path encoded in the agent's BOLT12 offer.

LNDK handles route discovery, onion encryption, and delivery confirmation.

### Step 3: Agent Lexe Node Responds

The agent's Lexe wallet returns a signed BOLT12 invoice with:
- A unique `payment_hash`
- The requested amount
- A blinded payment path back to the Lexe node
- 30-second expiry

### Step 4: Pay the Invoice

`SendPaymentV2` with `payment_request = bolt12_invoice`. LND routes through the network. Lexe node settles the HTLC and returns the preimage.

### Step 5: Preimage Storage and Idempotency

Preimage returned to the billing engine, which stores a `top_up_intervals` row with `payment_hash`, `preimage`, `agent_id`, `idempotency_key`, `amount_msats`, `settled_at`. The idempotency cache in the payment module also stores `(idempotency_key → (preimage, nil))` for 24h.

---

## BOLT11 Manual Top-Up Mode (Fallback)

If the agent's wallet does not support BOLT12 (no Lexe), the agent uses **periodic manual top-ups** instead of pull-pay:

1. At registration time the agent registers a `lightning_address` instead of a `bolt12_offer`
2. The agent uses `POST /v1/wallet/topup { amount_sats }` to receive a BOLT11 deposit invoice
3. The agent pays the invoice from their wallet
4. The platform settles the deposit (LND invoice subscription), credits `agents.balance_msats`
5. The billing engine meters the in-memory balance just like the BOLT12 case
6. When balance drops below threshold and the engine attempts `TopUpBalance`, the lookup discovers the agent has no `bolt12_offer` and returns `manual_topup_required`
7. The engine emits a `low_balance` WSS frame; the user is expected to manually top up
8. If balance reaches zero before manual top-up, the sandbox is paused (same as out-of-credits flow)

LNURL-pay is no longer used (it required per-tick HTTP requests to the agent's wallet server, which is slow and brittle). Manual top-up is the documented fallback.

### Offer Type Discrimination

The `agents` table stores:
- `offer_type`: `bolt12 | manual_bolt11`
- `bolt12_offer`: nullable, set for BOLT12 agents
- `lightning_address`: nullable, used for showing the agent's deposit address in the dashboard

The payment module checks `offer_type` before each `TopUpBalance` and routes accordingly.

---

## Error Classification

The error classes returned by `TopUpBalance` and `PayInvoice` drive the billing engine's behavior:

| Error Class | Examples | Billing Engine Action |
|---|---|---|
| `routing_failure` | No route, insufficient liquidity | Retry next cycle; increment consecutive_failures |
| `offer_unreachable` | Onion message delivery failed | Retry next cycle; increment consecutive_failures |
| `invoice_expired` | Agent's 30s invoice window missed | Immediate retry (regenerate); do not increment |
| `payment_rejected` | Agent explicitly rejects HTLC | Permanent failure; pause |
| `offer_invalid` | Offer decodes to invalid structure | Permanent failure; notify platform ops |
| `lnd_unavailable` | gRPC to LND lost (failover in progress) | Hold; reconnect; do not increment |
| `amount_below_minimum` | Offer minimum > requested | Increase chunk size; retry |
| `manual_topup_required` | Agent uses BOLT11 manual mode | Emit `low_balance` WSS; do not retry |

---

## Deposit (Manual Top-Up) Flow

### POST /v1/wallet/topup — Sequence

1. Handler calls `payment.CreateBOLT11Invoice(amount_sats, memo="deposit:{agent_id}:{amount}", expiry=3600)`
2. LND creates invoice; returns BOLT11 string and `payment_hash`
3. Store `pending_deposits` row in Cloud SQL: `{agent_id, payment_hash, amount_sats, status='pending', expires_at}`
4. Return BOLT11 to agent

### Deposit Settlement Detection

The payment module subscribes to LND's `SubscribeSingleInvoice` for each pending deposit. On settle:
1. `payment.OnInvoiceSettled` fires
2. Call `store.IncrementAgentBalance(agent_id, amount_msats)` (increments `agents.balance_msats`)
3. Update `pending_deposits` row: `status = 'settled'`
4. `NOTIFY agent_balance_change <agent_id>` so any open dashboard refreshes
5. If the agent has any sandboxes currently in `pause_pending` due to balance exhaustion, the billing engine offers them a resume opportunity (audit log: `billing.lightning.recovered`)

---

## Secret Management

All LND credentials are stored in GCP Secret Manager and mounted at container startup.

| Secret Name | Content | Mounted Path |
|---|---|---|
| `lnd-tls-cert` | LND TLS certificate (PEM, shared by HA pair) | `/run/secrets/lnd-tls.pem` |
| `lnd-invoice-macaroon` | Scoped LND macaroon (hex) | `/run/secrets/lnd-invoice.macaroon` |
| `lndk-config` | LNDK configuration | `/run/secrets/lndk.conf` |

---

## Monitoring & Alerting

### Metrics

| Metric | Type | Labels | Alert |
|---|---|---|---|
| `payment_lnd_active_node` | Gauge | node (primary/secondary) | Track failover events |
| `payment_lnd_connection_status` | Gauge | — (1=ok, 0=down) | Alert immediately if 0 for > 30s |
| `payment_lnd_failover_total` | Counter | direction | Operational |
| `payment_topup_duration_ms` | Histogram | success/failure | Alert P99 > 8000ms |
| `payment_topup_success_total` | Counter | — | Operational |
| `payment_topup_failure_total` | Counter | error_class | Alert if `routing_failure` > 100/min |
| `payment_bolt12_request_duration_ms` | Histogram | success/failure | Alert P99 > 6000ms |
| `payment_invoice_pool_size` | Gauge | — | Alert if < 100 (refill goroutine starvation) |
| `payment_invoice_pool_misses_total` | Counter | — | Alert if > 10/min |
| `payment_channel_balance_sats` | Gauge | — | Alert if < 1,000,000 sats |
| `payment_pending_htlcs` | Gauge | — | Alert if > 500 |
| `payment_idempotency_cache_hits_total` | Counter | — | Operational |

### Log Events

| Event | Level | When |
|---|---|---|
| `payment.lnd.connected` | INFO | Startup or failover completed |
| `payment.lnd.failover_started` | WARN | Failover detected |
| `payment.lnd.failover_completed` | INFO | Reconnected to new active |
| `payment.invoice_pool.refill_low` | WARN | Refill goroutine fell behind |
| `payment.bolt12.requested` | DEBUG | invoice_request sent |
| `payment.bolt12.settled` | DEBUG | Payment confirmed |
| `payment.bolt12.failed` | WARN | Payment failed (with reason) |
| `payment.bolt12.permanent_failure` | ERROR | Offer invalid or payment rejected |
| `payment.topup.idempotent_replay` | DEBUG | Same idempotency_key returned cached result |
| `payment.deposit.settled` | INFO | Agent top-up confirmed |
| `payment.channel_balance.low` | WARN | Balance < 2,000,000 sats |

---

## Dependencies

- **LND HA Pair** running on `10.0.0.5:10009` (active) and `10.0.0.6:10009` (standby)
- **LNDK sidecar** on the active LND VM
- **GCP Compute API** for `lnd-active` label discovery
- **GCP Secret Manager** for LND credentials
- **Store Module** — for agent offer lookup, deposit settlement, top-up audit rows
- **Billing Engine** (Component 3) — primary consumer of `TopUpBalance`
- **Native L402 Middleware** (Component 1) — consumes `MintL402Invoice` and `VerifyPreimage`
- **API handlers** — consume `CreateBOLT11Invoice` (deposit), `ValidateOffer` (registration)
