# Component 1: API Gateway & Native L402 (Cloud Run MVP)

## v3 changelog

- Removed global LB + Cloud Armor dependency from MVP.
- Removed `/h/{host_id}/*` routing model from control plane.
- Kept native Go auth middleware (L402 + JWT + API key).
- `data_jwt` now carries `pod_id` (not `host_id`).
- Data plane is direct to RunPod runtime endpoints.

---

## Overview

In MVP, the gateway is the Cloud Run control-plane service itself:

- one public HTTPS endpoint (`api.sockt.io`)
- in-process auth middleware
- no sidecars, no external proxy tier

The control plane handles lifecycle/auth requests. Sandbox exec/files/shell traffic goes directly to the pod runtime URL returned at create/resume.

---

## Auth Methods

| Method | Header | Use |
|---|---|---|
| L402 | `Authorization: L402 <macaroon>:<preimage>` | Lightning-native agents |
| API key | `Authorization: Bearer sockt_live_...` | credit path agents |
| User JWT | `Authorization: Bearer <jwt>` | dashboard/API-key management |

---

## Route Classification

### Control-plane routes

- `/v1/wallet/*`
- `/v1/sandbox/*` lifecycle
- `/v1/billing/*`
- `/v1/user/*`
- `/v1/mcp/*`

### Data-plane routes

No longer routed through control plane path prefixes.

SDK uses:

- `data_endpoint = https://<pod_id>-8443.proxy.runpod.net/sandbox/{id}`
- `shell_url = wss://<pod_id>-8443.proxy.runpod.net/sandbox/{id}/shell`

---

## Native L402 Middleware

L402 verification remains in-process:

1. parse macaroon + preimage
2. verify macaroon signature chain
3. verify preimage hash
4. validate caveats (expiry, service, sandbox)
5. place principal in request context

No architectural change beyond lower invoice pool size.

---

## `data_jwt` Claims

| Claim | Value |
|---|---|
| `iss` | `sockt-cp` |
| `aud` | `sockt-runtime` |
| `sub` | `sandbox_id` |
| `pod_id` | runpod pod id |
| `principal_kind` | `l402` or `api_key` |
| `scope` | `exec`,`files`,`shell`,`status` |
| `exp` | short-lived (default 1h) |

Runtime rejects token if `pod_id` does not match local pod identity.

---

## L402 Invoice Pool (MVP)

- target size: 100
- refill threshold: 40
- fixed 1-sat onboarding invoices
- fallback sync mint when empty

---

## Error Schema

Unchanged shape:

```json
{
  "code": 401,
  "error": "invalid_api_key",
  "message": "API key is invalid",
  "request_id": "uuid"
}
```

Additional MVP-centric errors:

- `pod_unreachable`
- `pod_replaced`
- `runpod_capacity_unavailable`

---

## Rate Limiting

MVP approach:

- app-level per-principal token buckets
- optional Cloudflare rate-limit rules if enabled

No Cloud Armor dependency in MVP.

---

## Deployment Notes

- deploy as Cloud Run service
- custom domain mapping for `api.sockt.io`
- auth, routing, and error handling all in app code

---

## Dependencies

- Payment module (Component 5)
- Supabase backend (Component 6)
- Orchestrator (Component 4)
- Runtime pod auth contract (Component 8)

---

## Phase 2 (post-MVP)

Reintroduce edge stack if needed:

- global external LB
- Cloud Armor policies
- path-based split to internal backends
