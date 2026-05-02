# Component 5: Payment Module (Single-LND MVP)

## v3 changelog

- Replaced LND HA pair with one LND node on `e2-small`.
- Dropped `lnd-active` discovery and failover logic from MVP.
- Switched to Neutrino mode (no local bitcoind).
- Reduced pre-minted L402 invoice pool target from 1000 to 100.
- Kept BOLT12 top-up + idempotency interface unchanged.

---

## Overview

The payment module (`internal/payment`) remains the only Lightning integration point, but now targets a single LND instance for ultra-low fixed cost.

Responsibilities:

- validate/register offers
- mint L402 invoices
- execute BOLT12 top-ups
- settle manual BOLT11 deposits
- surface node health

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

Single point of failure: if this node is down, top-ups pause until recovery.

---

## Primary APIs

- `ValidateOffer(offer)`
- `TopUpBalance(ctx, agentID, sats, idempotencyKey)`
- `RequestBOLT12Invoice(ctx, offer, amountSats, memo)`
- `PayInvoice(ctx, invoice, idempotencyKey)`
- `CreateBOLT11Invoice(ctx, amountSats, memo, expiry)`
- `MintL402Invoice()`
- `GetNodeHealth(ctx)`
- `VerifyPreimage(preimage, paymentHash)`

---

## Top-Up Flow

1. Resolve offer by agent.
2. Build BOLT12 invoice request.
3. Receive invoice from agent wallet.
4. Pay invoice through LND.
5. Record settlement (`top_up_intervals`).
6. Return preimage + amount to caller.

Idempotency:

- app-level cache by `idempotencyKey` (24h)
- protocol-level safety via payment hash dedupe

---

## L402 Invoice Pool

Target pool size: **100** entries.

Policy:

- refill when size < 40
- 1-sat fixed amount invoices
- 1-hour expiry
- cancel stale invoices where possible

This keeps memory footprint low for Cloud Run while preserving low-latency 402 responses.

---

## Error Classification

| Error | Action |
|---|---|
| `routing_failure` | retry later |
| `offer_unreachable` | retry later |
| `invoice_expired` | immediate regenerate/retry |
| `payment_rejected` | pause sandbox |
| `offer_invalid` | mark permanent failure |
| `lnd_unavailable` | transient, retry with backoff |
| `manual_topup_required` | notify user, do not auto-retry |

---

## Deposit Flow (Manual Top-Up)

1. Create BOLT11 invoice.
2. Store pending deposit row.
3. Subscribe/poll invoice settlement.
4. On settle:
   - increment agent balance
   - mark deposit settled
   - publish balance event

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

- top-up success/failure rate
- top-up latency p95/p99
- invoice pool size + misses
- channel balance
- LND connection health

Alert on:

- connection unavailable > 60s
- top-up failures sustained
- pool size near zero

---

## Dependencies

- Billing callbacks (Component 3)
- Supabase backend (Component 6)
- API auth middleware (Component 1)

---

## Phase 2 (post-MVP)

Restore v2 Lightning architecture:

- active/passive LND pair
- failover daemon + active-label discovery
- larger invoice pool
- optional local bitcoind backend
