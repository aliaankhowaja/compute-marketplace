# Component 5: Payment Module (Single-LND MVP)

## v4 changelog

- **Prepaid BOLT11 only for agent balance:** Sockt mints invoices; agents pay Sockt out-of-band (wallet / internal liquidity). Sockt **does not** pay agents via BOLT12, LNURL, or pull-pay.
- Dropped autonomous `TopUpBalance` / agent-offer-driven pull from the MVP control plane path.
- `RequestDepositInvoice` + pending-deposit bookkeeping + indexer/settlement callbacks remain the Lightning funding path aligned with billing.

---

## v3 changelog (historical)

- Replaced LND HA pair with one LND node on `e2-small`.
- Dropped `lnd-active` discovery and failover logic from MVP.
- Switched to Neutrino mode (no local bitcoind).
- Reduced pre-minted L402 invoice pool target from 1000 to 100.

---

## Overview

The payment module (`internal/payment`) is the only Lightning integration point for the control plane, targeting a single LND instance.

Responsibilities:

- mint L402 challenge invoices (pool)
- mint **deposit** BOLT11 invoices (`RequestDepositInvoice`) and record pending rows
- detect settlement and credit agent balance (with idempotency)
- optional direct `PayInvoice` for platform ops (unchanged contract)
- surface node health (`GetNodeHealth`)

**Not** in MVP anymore: validating/storing agent BOLT12 offers, LNURL webhook pull-pay, Sockt initiating payment to agent nodes for “top-up.”

---

## LND Topology (MVP)

| Property | Value |
|---|---|
| Node count | 1 |
| VM | GCE `e2-small` |
| LND mode | Neutrino |
| gRPC endpoint | private VPC IP + port `10009` |
| Resilience | restart-on-failure only |

### Explicit risk

Single point of failure: if this node is down, **new deposits and L402 issuance** stall until recovery; already-funded balances still meter offline until callbacks fail.

---

## Primary APIs (`internal/payment.Payment`)

- `MintL402Invoice()` / pool maintenance
- `RequestDepositInvoice(ctx, agentID, amountSats, expirySeconds, memo, idempotencyKey)` — mint BOLT11 + persist pending deposit
- `PayInvoice(ctx, bolt11, idempotencyKey)` — when the platform pays an invoice
- `CreateBOLT11Invoice` / decoding helpers (`DecodeBolt11`, `LooksLikeBolt11Invoice`) as implemented
- `GetNodeHealth(ctx)`
- `VerifyPreimage(preimage, paymentHash)`

---

## Deposit / refill flow (Lightning agents)

1. Agent (or MCP/SDK) hits `POST /v1/wallet/deposit` (or runtime extend-balance / topup-trigger responds with **`deposit_required`**).
2. Control plane mints BOLT11, stores `pending_deposit`, returns `bolt11`, `payment_hash`, `expires_at`.
3. Agent pays invoice from **their** wallet.
4. LND / hooks mark invoice settled → `SettlePendingDeposit` (+ balance increment).
5. Wallet balance poll + MCP `wallet_deposit_status` + WSS **`low_balance` / `deposit_required`** hints keep UX in sync.

Idempotency:

- app-level cache by `idempotency_key` (24h TTL) replays identical mint responses
- payment-hash dedupe in store

---

## L402 Invoice Pool

Target pool size: **100** entries.

Policy:

- refill when size < 40
- 1-sat fixed amount invoices
- 1-hour expiry
- cancel stale invoices where possible

---

## Error Classification

| Error | Action |
|---|---|
| `routing_failure` | retry later |
| `invoice_expired` | regenerate deposit |
| `payment_rejected` | user-facing; may pause sandbox |
| `lnd_unavailable` | transient, retry with backoff |
| `invoice_failed` / `deposit_mint_failed` | surface to client; retry mint |

---

## Secrets

Required:

- `lnd-tls-cert`
- `lnd-macaroon`

Optional:

- `lnd-node-pubkey` for diagnostics

---

## Monitoring

Track:

- deposit mint success/failure rate
- time-to-settle for pending deposits
- invoice pool size + misses
- channel balance / inbound liquidity
- LND connection health

Alert on:

- connection unavailable > 60s
- sustained mint failures
- pool size near zero

---

## Dependencies

- Billing / runtime callbacks (Component 3)
- Supabase backend (Component 6)
- API auth middleware (Component 1)

---

## Phase 2 (post-MVP)

- active/passive LND pair + failover
- larger invoice pool
- optional local bitcoind backend
- reconsider pull-pay models only if product requires them
