# Component 5: Payment Module

## Overview

The Payment Module is the `internal/payment` package within the unified Go binary. It abstracts all Lightning Network interactions into a clean interface consumed by the Billing Engine, the API handlers, and Aperture (for L402). It manages two Lightning implementations: LND (the platform's own node) and the BOLT12 onion messaging layer via LNDK.

The module is the only component in the system that communicates directly with the Lightning Network. All other modules call the `Payment` interface.

---

## Responsibilities

- Maintain a persistent gRPC connection to the platform LND node
- Issue BOLT12 invoice requests to agent offers via onion messaging (LNDK)
- Pay BOLT12 invoices returned by agent Lexe nodes
- Issue BOLT11 invoices for L402 authentication challenges (via Aperture)
- Issue BOLT11 deposit invoices for agent top-ups
- Validate BOLT12 offer strings before storage
- Report node channel balance and routing health
- Manage BOLT12 offer registration and caching

---

## Lightning Node Infrastructure

### Platform LND Node

The platform operates a single LND node (upgradeable to a cluster) running on a dedicated GCP VM in the `sc-control` subnet at `10.0.0.5`.

**LND node specifications:**
- LND version: 0.18+ (minimum for full channel reliability)
- LNDK sidecar: running alongside LND for BOLT12 onion message support
- Bitcoin backend: Bitcoin Core running on the same VM (pruned node, only needs block headers + wallet)
- Channel policy: auto-managed by LND's autopilot with manual overrides for key routing nodes
- Minimum channel size: 5,000,000 sats (to handle burst payments at high sandbox concurrency)
- Total channel capacity target: 100,000,000 sats (1 BTC) across 10+ well-connected routing peers

### LNDK Sidecar

LNDK adds BOLT12 onion messaging to LND without replacing it. It acts as a sidecar process that:
- Intercepts onion messages arriving at the LND node
- Parses BOLT12 `invoice_request` messages addressed to the platform node
- Passes `invoice_request` messages from the platform to agent offer nodes
- Returns BOLT12 invoices from agents back to the calling LND client

**LNDK is required for BOLT12 offer negotiation.** Without it, the platform would need to use Core Lightning (CLN) instead of LND.

### Alternative: Core Lightning (CLN)

If LNDK proves unstable or insufficient, the payment module is designed to swap the backend to CLN with minimal interface changes:
- CLN has native BOLT12 support enabled by default (`--enable-experimental-features`)
- CLN's gRPC API (`lightning.proto`) differs from LND's but the `Payment` interface abstracts this
- The Aperture L402 proxy requires configuration changes to use CLN instead of LND

---

## Payment Interface

All operations available to other modules:

### Offer Management

**ValidateOffer(offer string) → error**
- Decodes the BOLT12 offer string and checks structural validity
- Does not contact the network — purely local validation
- Returns error if: offer is malformed, prefix does not match expected network (`lno1` for mainnet, `lno1qsg` for testnet)

**CacheOffer(agentID UUID, offer string)**
- Stores the offer string in an in-memory LRU cache (capacity: 100,000 entries)
- Cache key: `agentID`
- TTL: 1 hour (refreshed on use)
- Reduces Supabase reads for billing tickers that call `RequestBOLT12Invoice` every 10 seconds

### Invoice Operations

**RequestBOLT12Invoice(ctx, offer string, amountSats int64, memo string) → (BOLT12Invoice, error)**
- Sends an `invoice_request` to the agent's offer via LNDK onion messaging
- The agent's Lexe node receives the `invoice_request`, generates a unique BOLT12 invoice, and returns it via onion message
- Timeout: 8 seconds (must complete within billing interval window)
- Returns the signed BOLT12 invoice string on success

**PayInvoice(ctx, invoice string) → (Preimage, error)**
- Pays either a BOLT11 or BOLT12 invoice using LND's `SendPaymentV2` RPC (router service)
- Returns the 32-byte preimage on success — this is the cryptographic proof of payment
- Timeout: configurable, default 25 seconds
- On routing failure: returns structured error with `failure_reason` (no_route, insufficient_balance, timeout, etc.)

**CreateBOLT11Invoice(ctx, amountSats int64, memo string, expirySeconds int) → (BOLT11Invoice, error)**
- Creates a new BOLT11 invoice on the platform's LND node
- Used by: Aperture for L402 authentication challenges, agent top-up deposits
- Memo format for L402: `"L402:{route}:{request_id}"`
- Memo format for top-ups: `"deposit:{agent_id}:{amount_sats}"`

**GetInvoiceStatus(ctx, paymentHash string) → InvoiceStatus**
- Checks whether a BOLT11 invoice has been paid
- Returns: `pending | settled | expired | cancelled`
- Used by: agent top-up polling (agent pays externally and platform polls for settlement)

### Balance & Health

**GetNodeBalance(ctx) → BalanceInfo**
- Returns current LND node financial state:
  - `wallet_balance_sats` — on-chain balance
  - `channel_balance_sats` — total Lightning channel balance
  - `pending_open_sats` — balance in pending channels
  - `pending_htlc_sats` — in-flight HTLCs
  - `active_channels` — count of active channels
  - `inactive_channels` — count of inactive channels

**GetNodeHealth(ctx) → NodeHealth**
- Checks connectivity: gRPC connection to LND, LNDK sidecar responsiveness
- Used by `/healthz` endpoint
- Returns: `{ lnd_connected: bool, lndk_connected: bool, synced_to_chain: bool, block_height: int }`

**VerifyPreimage(preimage string, paymentHash string) → bool**
- Stateless local verification: checks `sha256(preimage) == paymentHash`
- Used by Aperture for L402 verification (Aperture calls this via Go function, not HTTP)

---

## LND gRPC Client Configuration

The LND gRPC connection is established at binary startup and maintained for the lifetime of the process.

### Connection Parameters

| Parameter | Value |
|---|---|
| Target | `10.0.0.5:10009` |
| TLS cert | Mounted from GCP Secret Manager at startup |
| Macaroon | Scoped macaroon: `invoices:read`, `invoices:write`, `offchain:read`, `offchain:write` |
| Max recv message size | 16 MB |
| Keepalive interval | 30 seconds |
| Keepalive timeout | 10 seconds |
| Reconnect backoff | Exponential (initial: 1s, max: 30s) |

### LND gRPC Services Used

| Service | Methods Used |
|---|---|
| `lnrpc.Lightning` | `GetInfo`, `ChannelBalance`, `WalletBalance`, `AddInvoice`, `LookupInvoice` |
| `routerrpc.Router` | `SendPaymentV2`, `TrackPaymentV2` |
| `lnrpc.Lightning` (BOLT12 via LNDK) | `SendMessage` (onion message), `SubscribeCustomMessages` |
| `invoicesrpc.Invoices` | `SubscribeSingleInvoice` (for top-up detection) |

---

## BOLT12 Offer-to-Payment Sequence Detail

### Step 1: invoice_request Construction

The payment module constructs a BOLT12 `invoice_request` with:
- `offer`: the agent's stored offer string
- `amount_msat`: `sats_owed × 1000`
- `payer_note`: `"sandbox:{sandbox_id}:tick:{tick_number}"` — for agent-side reconciliation
- `payer_id`: the platform node's pubkey
- `created_at`: current Unix timestamp
- `payer_metadata`: opaque bytes (can include sandbox_id for agent audit)

### Step 2: Send via Onion Message

The `invoice_request` is encoded as a TLV (Type-Length-Value) blob and sent via LNDK as an onion message to the blinded path encoded in the agent's BOLT12 offer.

**LNDK handles:**
- Route discovery to the agent's offer node via blinded paths
- Onion encryption of the message
- Delivery confirmation

### Step 3: Agent Lexe Node Responds

The agent's Lexe wallet (always-online Lightning node) receives the onion message, validates the `invoice_request`, and returns a BOLT12 invoice via a reply onion message. The invoice contains:
- A unique payment hash (different from the offer's key)
- The requested amount
- A blinded payment path back to the Lexe node
- Expiry: 30 seconds (tight expiry prevents double-payment attacks)

### Step 4: Pay the Invoice

The payment module calls LND's `SendPaymentV2` RPC with the returned BOLT12 invoice. LND routes the payment through the Lightning Network. The Lexe node settles the HTLC and returns the preimage.

### Step 5: Preimage Storage

The preimage (32 bytes, hex-encoded) is returned to the billing engine, which stores it in `billing_intervals.bolt12_preimage`. This is cryptographic proof that the agent's Lexe node confirmed payment receipt.

---

## BOLT11 Fallback Mode

If the agent's wallet does not support BOLT12 (no Lexe, uses a BOLT11-only wallet):

1. At offer registration, the agent provides a **Lightning Address** instead of a BOLT12 offer
   - Lightning Addresses follow the format `agent@wallet.example.com`
   - The platform resolves this via LNURL-pay to fetch a BOLT11 invoice per tick
2. For each billing tick, the payment module:
   - Makes an HTTP request to `https://wallet.example.com/.well-known/lnurlp/agent`
   - Sends a pay request with `amount = sats_owed × 1000 msat`
   - Receives a BOLT11 invoice
   - Pays the BOLT11 invoice via LND `SendPaymentV2`
3. The rest of the billing flow is identical

**Fallback limitations:**
- LNURL-pay requires the agent's wallet server to be publicly accessible (no blinded paths)
- No receipt proof beyond the standard BOLT11 preimage
- Higher latency than BOLT12 onion messaging

### Offer Type Discrimination

The `agents` table stores:
- `offer_type`: `bolt12 | lnurl | lightning_address`
- `bolt12_offer`: nullable, set for BOLT12 agents
- `lightning_address`: nullable, set for fallback agents

The payment module checks `offer_type` at the start of each tick and routes to the appropriate payment path.

---

## Error Classification

Payment errors are classified into categories that drive different retry and escalation behavior:

| Error Class | Examples | Billing Engine Action |
|---|---|---|
| `routing_failure` | No route, insufficient liquidity | Retry next tick; increment consecutive_failures |
| `offer_unreachable` | Onion message delivery failed, timeout | Retry next tick; increment consecutive_failures |
| `invoice_expired` | Agent's 30s invoice window missed | Immediate retry (regenerate invoice_request); do not increment failures |
| `payment_rejected` | Agent explicitly rejects HTLC | Permanent failure; suspend sandbox |
| `offer_invalid` | Offer decodes to invalid structure | Permanent failure; notify platform ops |
| `lnd_unavailable` | gRPC connection to LND lost | Hold all tickers; alert; reconnect; resume |
| `amount_below_minimum` | Agent's offer has minimum amount > sats_owed | Calculate multi-tick batch: accumulate sats until minimum met |

### Minimum Amount Batching

Some BOLT12 offers specify a minimum payment amount (e.g., 100 sats minimum). If `tier.sats_per_second × billing_interval_secs < offer_minimum_sats`, the billing engine accumulates ticks without payment until the balance reaches the minimum, then settles in one BOLT12 payment covering multiple intervals.

This is tracked in the ticker's `accumulatedSats` field and reflected in a single `billing_interval` row covering the entire accumulated duration.

---

## Deposit (Top-Up) Flow

Agents who need to pre-fund their balance (used in fallback scenarios where BOLT12 pull-pay is unavailable) can request a BOLT11 deposit invoice.

### POST /v1/wallet/topup — Sequence

1. Handler calls `payment.CreateBOLT11Invoice(amount_sats, memo, expiry=3600)`
2. LND creates invoice, returns BOLT11 string and payment_hash
3. Store `pending_deposit` row in Supabase: `{agent_id, payment_hash, amount_sats, status='pending', expires_at}`
4. Return BOLT11 to agent

### Deposit Settlement Detection

The payment module subscribes to LND's `SubscribeSingleInvoice` RPC for each pending deposit invoice. When the invoice settles:
1. `payment.OnInvoiceSettled` callback fires
2. Call `store.UpdateAgentBalance(agent_id, +amount_sats)`
3. Update `pending_deposits` row: `status = 'settled'`
4. Emit Supabase Realtime event on `agents` table update

---

## Secret Management

All LND credentials are stored in GCP Secret Manager and mounted as files at container startup. The payment module reads them at initialization.

| Secret Name | Content | Mounted Path |
|---|---|---|
| `lnd-tls-cert` | LND TLS certificate (PEM) | `/run/secrets/lnd-tls.pem` |
| `lnd-invoice-macaroon` | Scoped LND macaroon (hex) | `/run/secrets/lnd-invoice.macaroon` |
| `lndk-config` | LNDK configuration file | `/run/secrets/lndk.conf` |

---

## Monitoring & Alerting

### Metrics

| Metric | Type | Labels | Alert |
|---|---|---|---|
| `payment_bolt12_request_duration_ms` | Histogram | success/failure | Alert P99 > 6000ms |
| `payment_bolt12_pay_duration_ms` | Histogram | success/failure | Alert P99 > 20000ms |
| `payment_failures_total` | Counter | error_class | Alert if routing_failure > 100/min |
| `payment_lnd_connection_status` | Gauge | — (1=ok, 0=down) | Alert immediately if 0 |
| `payment_channel_balance_sats` | Gauge | — | Alert if < 1,000,000 sats |
| `payment_pending_htlcs` | Gauge | — | Alert if > 500 |
| `payment_bolt11_deposits_pending` | Gauge | — | Monitor |

### Log Events

| Event | Level | When |
|---|---|---|
| `payment.lnd.connected` | INFO | Startup |
| `payment.lnd.reconnecting` | WARN | gRPC connection lost |
| `payment.bolt12.requested` | DEBUG | invoice_request sent |
| `payment.bolt12.settled` | DEBUG | Payment confirmed |
| `payment.bolt12.failed` | WARN | Payment failed with reason |
| `payment.bolt12.permanent_failure` | ERROR | Offer invalid or payment rejected |
| `payment.deposit.settled` | INFO | Agent top-up confirmed |
| `payment.channel_balance.low` | WARN | Balance < 2,000,000 sats |

---

## Dependencies

- **LND node** running on GCP VM at `10.0.0.5:10009`
- **LNDK sidecar** running alongside LND for BOLT12 support
- **GCP Secret Manager** for LND credentials
- **Store Module** — for agent offer lookup and deposit status updates
- **Billing Engine** (Component 3) — primary consumer of `RequestBOLT12Invoice` and `PayInvoice`
- **API handlers** — consume `CreateBOLT11Invoice` (top-up), `ValidateOffer` (registration)
