# Sockt MVP — Build & deployment checklist

Expanded from [00-index.md](./00-index.md) § Build Order. Check items as you complete them.

---

## Phase 0 — Foundations

- [.] Create GCP project and attach billing; set default region (e.g. `us-central1`)
- [.] Create VPC `sc-network` and subnet `sc-lnd` (`10.0.0.0/24`)
- [.] Enable APIs: Cloud Run, Compute Engine, Secret Manager, Cloud Logging/Monitoring
- [.] Create Supabase project (Postgres, Auth, Realtime, Storage)
- [ ] Create RunPod org; generate API key; choose initial region allowlist
- [ ] Create Stripe account; obtain webhook secret (credits path)
- [ ] Configure GitHub repo + GHCR; enable GitHub Actions
- [ ] Reserve DNS for `api.sockt.dev` (optional: Cloudflare free)
- [ ] Create Secret Manager entries (placeholders OK): `supabase-url`, `supabase-service-role-key`, `runpod-api-key`, `lnd-tls-cert`, `lnd-macaroon`, `wss-jwt-secret`, `stripe-webhook-secret`

**Milestone:** Accounts and projects exist; no app deployed yet.

---

## Phase 1 — Database & schema (Component 06)

- [ ] Apply migrations: `users`, `agents`, `api_keys`
- [ ] Apply migrations: `templates`, `tier_pricing`
- [ ] Apply migrations: `sandboxes`
- [ ] Apply migrations: `billing_intervals`, `credit_transactions`
- [ ] Apply migrations: `top_up_intervals`, `pending_deposits`, `audit_log`
- [ ] Seed `tier_pricing` (nano, micro, standard, gpu_small, gpu_large)
- [ ] Seed `templates` for default RunPod CPU/GPU images
- [ ] Configure Supabase Auth for dashboard users
- [ ] Configure Supavisor: transaction pool (`:6543`) → `SUPABASE_DB_TX_DSN`
- [ ] Configure Supavisor: session pool (`:5432`) → `SUPABASE_DB_SESSION_DSN`
- [ ] Enable Realtime channels: `apikey_revoked`, `agent_balance_change`, `user_warning_level`, `sandbox_state`
- [ ] Create Storage bucket `lnd-backups`

**Milestone:** Schema queryable from dev machine using both pool DSNs.

---

## Phase 2 — Lightning node (Components 09 + 05)

- [.] Provision GCE VM `lnd-node-mvp` (`e2-small`, `pd-balanced` 30 GB, static internal IP in `sc-lnd`)
- [ ] Install LND (Neutrino); configure `lnd.conf`; initialize wallet/seed
- [ ] Open channels with adequate **inbound** liquidity for prepaid agent deposits + outbound for L402 as needed
- [ ] Export `channel.backup` every 5 min → upload to Supabase Storage `lnd-backups`
- [ ] Store `lnd-tls-cert`, `lnd-macaroon` (optional: `lnd-node-pubkey`) in Secret Manager
- [ ] Optional: health sidecar with `/healthz` for alerting

**Milestone:** `lncli getinfo` works; can mint BOLT11 and exercise BOLT12 offers from VPC.

---

## Phase 3 — Runtime image `sockt-runtime` (Components 02 + 08 + 03)

- [ ] `config` — env: `SANDBOX_ID`, `POD_ID`, `CONTROL_PLANE_URL`, JWT/billing secrets, rates
- [ ] `auth` — verify `data_jwt` (claims, `pod_id` matches local pod, scope, expiry)
- [ ] `exec`, `files`, `status` — HTTPS on `:8443` per plan routes
- [ ] `shell` — WSS with PTY and message types from Component 08
- [ ] `meter` — 1s tick, local balance, audit buffer, billing frame fan-out
- [ ] `callbacks` — `audit-batch`, `topup-trigger`, `pause-trigger` to control plane
- [ ] `server` — wire HTTPS + WSS + meter + callbacks; `/healthz`
- [ ] Docker image → GHCR (tag by commit SHA)
- [ ] RunPod template: image + port `8443` exposed

**Milestone:** Manual pod launch; hit status/exec with test `data_jwt`; WSS smoke test.

---

## Phase 4 — Control plane `sockt-cp` (Components 02, 06, 05, 04, 03, 01, 07)

### 4.1 Bootstrap

- [ ] `internal/config` + `cmd/control-plane/main.go` skeleton
- [ ] `/healthz`; deploy to Cloud Run smoke (validates project + networking)

### 4.2 Store (06)

- [ ] `pgx` pools (txn primary, session for advisory-lock paths)
- [ ] Repos for users, agents, api_keys, sandboxes, tier_pricing, billing tables
- [ ] Pool tuning per plan (`MaxConns`, `MinConns`, lifetimes)

### 4.3 Auth (01)

- [ ] L402 middleware (macaroon, preimage, caveats)
- [ ] JWT verification (Supabase + app minted)
- [ ] API key Bearer + cache + `apikey_revoked` realtime
- [ ] `data_jwt` minting (`iss`, `aud`, `sub`, `pod_id`, `principal_kind`, `scope`, `exp`)

### 4.4 Payment (05)

- [ ] LND gRPC client (single node)
- [ ] Deposit mint + settle path (`pending_deposits`, balance credit), L402 pool (100 target, refill <40), idempotency store
- [ ] Error classification per plan

### 4.5 Orchestrator (04)

- [ ] RunPod client: deploy, stop, resume, terminate, volumes, get pod
- [ ] Lifecycle: create / pause / resume / terminate / endpoint resolution
- [ ] Persist `pod_id`, `pod_proxy_url`, region, volume, resume metadata
- [ ] MVP error mapping (`pod_unreachable`, `pod_starting`, etc.)

### 4.6 Billing ingest (03)

- [ ] `POST /v1/runtime/billing/audit-batch`
- [ ] `POST /v1/runtime/topup-trigger`
- [ ] `POST /v1/runtime/pause-trigger`
- [ ] Reconciliation admin endpoint (+ external cron hook later)

### 4.7 API router (01 + 02)

- [ ] `/v1/wallet/*`, `/v1/sandbox/*`, `/v1/billing/*`, `/v1/user/*`
- [ ] `/v1/sandbox/{id}/refresh-data-jwt`
- [ ] Callback routes from 4.6
- [ ] Canonical error schema + MVP-specific codes
- [ ] App-level per-principal rate limits

### 4.8 MCP (07)

- [ ] Streamable HTTP under `/v1/mcp/*`
- [ ] Tool set: create, exec, files, status, lifecycle, extend_balance, wallet_balance
- [ ] Stateless sessions; reconnect semantics; middleware auth

### 4.9 Cloud Run (09)

- [ ] Service settings: CPU/mem, min=0, max=10, concurrency 80, timeout 900s
- [ ] Direct VPC egress to `sc-network` (LND private reachability)
- [ ] Public ingress; app-level auth
- [ ] Custom domain mapping `api.sockt.dev`

**Milestone:** Create sandbox → pod runs → `data_endpoint` + `shell_url` returned.

---

## Phase 5 — End-to-end integration

- [ ] L402 path: verify macaroon → principal → `data_jwt`
- [ ] API key path: dashboard key → create sandbox
- [ ] Exec + files against RunPod `data_endpoint`
- [ ] WSS shell + live `billing` frames from runtime meter
- [ ] Low balance → `topup-trigger` → BOLT12 → delta applied; `topup` frame on shell
- [ ] Pause / resume / terminate; redeploy fallback updates `pod_id` and JWT
- [ ] Credits path: Stripe webhook → credits → runtime continues
- [ ] Reconciliation cron + discrepancy checks

**Milestone:** Full Lightning and credits loops stable in staging.

---

## Phase 6 — Agent SDK (10)

- [ ] Python: `runpod_cpu` / `runpod_gpu`, `pod_id`, endpoints, JWT refresh rules
- [ ] Python: exec/files/shell + errors per plan (`HostUnreachableError`, etc.)
- [ ] TypeScript: parity with Python
- [ ] `@sockt/mcp-server` proxy (stdio ↔ Streamable HTTP)
- [ ] Integration tests: URL shape, resume new pod, refresh on restart

**Milestone:** Published SDKs exercise production-like API.

---

## Phase 7 — CI/CD, observability, launch hardening (09)

- [ ] GitHub Actions: test → build CP + runtime images → GHCR → `gcloud run deploy` → smoke `/healthz`
- [ ] RunPod template pin/update on successful deploy
- [ ] Alerts: Cloud Run 5xx, LND health, Supabase connectivity, RunPod failures, reconcile discrepancies
- [ ] Metrics: runtime (`runtime_*`, `wss_*`), orch (`orch_runpod_*`), billing (`billing_*`), MCP (`mcp_*`)
- [ ] External cron for reconciliation (e.g. `cron-job.org`)
- [ ] Secret rotation runbook
- [ ] Optional: Cloudflare in front of `api.sockt.dev`

**Milestone:** Production launch checklist complete.

---

## Parallel work (optional scheduling)

- [ ] Run Phases 1, 2, 3 in parallel (schema, LND, runtime image)
- [ ] In Phase 4: `store` + `auth` parallel with `payment` + `orchestrator` until billing/API merge
- [ ] Start Phase 7 CI pipeline after Phase 4.1 lands on `main`
- [ ] Python and TypeScript SDKs in parallel (Phase 6)

---

## Explicitly out of MVP (Phase 2)

Do not block MVP on:

- [ ] `internal/cluster`, memberlist, consistent-hash ownership
- [ ] Firecracker host pool, host-agent gRPC, internal LB
- [ ] Cloud SQL; `instance_leases`; `rate_limit_buckets`; heavy partitioning
- [ ] Global LB + Cloud Armor; Cloud NAT; HTTP/3 edge tuning
- [ ] LND HA pair; large invoice pool; local bitcoind
- [ ] Centralized control-plane billing ticker
- [ ] KMS-wrapped per-sandbox DEK for SSH

---

## Plan cross-reference

| Phase / area | Plan files |
|--------------|------------|
| Index & overview | [00-index.md](./00-index.md) |
| API / auth / `data_jwt` | [01-api-gateway-l402-proxy.md](./01-api-gateway-l402-proxy.md) |
| Binaries & routes | [02-unified-go-binary.md](./02-unified-go-binary.md) |
| Billing (runtime + callbacks) | [03-billing-engine.md](./03-billing-engine.md) |
| RunPod orchestration | [04-sandbox-orchestrator.md](./04-sandbox-orchestrator.md) |
| LND / payments | [05-payment-module.md](./05-payment-module.md) |
| Supabase | [06-supabase-backend.md](./06-supabase-backend.md) |
| MCP | [07-mcp-server.md](./07-mcp-server.md) |
| WebSocket shell | [08-websocket-shell-server.md](./08-websocket-shell-server.md) |
| GCP / RunPod / CI | [09-gcp-infrastructure.md](./09-gcp-infrastructure.md) |
| SDK | [10-agent-sdk.md](./10-agent-sdk.md) |
