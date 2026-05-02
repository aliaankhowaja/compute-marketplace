# Component 2: Control-Plane Binary + Host-Agent Binary

## v2 changelog

- The single Go binary is split into **two** binaries built from the same monorepo:
  - `cmd/control-plane/` — runs on `mig-control-plane` GCE instances. Owns auth, sandbox lifecycle, billing meter, payment, MCP, REST API.
  - `cmd/host-agent/` — runs on every Firecracker host VM and every dedicated GPU VM. Owns Firecracker lifecycle, vsock proxy, exec/files/shell data plane, and per-sandbox in-process balance cache.
- New `internal/cluster/` module: cluster membership (memberlist over UDP/TCP 7444) + consistent-hash ring keyed on `sandbox_id`. Each sandbox has exactly one owning instance at any time; non-owner instances forward requests over internal gRPC.
- All Aperture trust headers removed; auth context lives only in the Go request context.
- `HandleCreateSandbox` response now includes `data_endpoint` (URL pointing at the host-data internal LB path for this sandbox's host) and `data_jwt` (short-lived signed JWT scoped to this sandbox).
- All sandbox data-plane endpoints (`exec`, `files`, `shell`, `status`) move out of the control plane and into the host-agent (defined in this file).
- Crash recovery now restarts only the **balance cache and meter** for sandboxes whose ownership hashes back to this instance — not all running sandboxes globally.

---

## Overview

The control plane is the system's "brain": it accepts external HTTP/MCP traffic, authenticates and authorises requests, creates and destroys sandboxes, talks to LND for payments, and runs the in-memory billing meter. It is a stateless-by-design Go binary horizontally scaled by `mig-control-plane` (Component 9), with cluster membership and consistent hashing providing per-sandbox state pinning.

The host-agent is the "muscle": one Go binary running on every host VM. It is the **only** thing that touches Firecracker, vsock, or the guest filesystem, and it terminates the sandbox data plane (exec, files, shell) directly so the control plane is never on the hot path for these operations.

Both binaries share the same `internal/store/`, `internal/proto/`, and many domain types. They are built from a single monorepo to keep types in lockstep.

---

## Design Philosophy

### Why the Two-Binary Split

A single binary on Cloud Run had three structural problems: (1) every sandbox operation went through the control plane, paying VPC-connector latency on the way to the host; (2) in-process state (billing tickers, WSS fan-out) silently broke under autoscale; (3) Cloud Run's CPU throttling stopped the billing tickers between requests.

Splitting into a control plane and a per-host data plane solves all three:
- The sandbox data plane terminates on the host VM that runs the sandbox; latency drops to in-VPC TCP plus a vsock hop.
- Control-plane state is sharded by consistent hash; in-process state is correct under scale-out.
- Both binaries run on dedicated GCE instances with no CPU throttling.

### Why Still a Modular Monolith Inside Each Binary

In-process function calls between modules are still microseconds, vs. milliseconds for inter-service HTTP. The control plane's billing meter calls `payment.TopUpBalance` directly, the orchestrator calls `cluster.OwnerOf` directly, etc. Modules can be extracted later if a single module becomes a scaling bottleneck.

---

## Repository Structure

```
sockt/
├── cmd/
│   ├── control-plane/
│   │   └── main.go                  # Entry point: wires control-plane modules
│   └── host-agent/
│       └── main.go                  # Entry point: wires host-agent modules
├── internal/
│   ├── api/                         # HTTP handler layer (control plane only)
│   │   ├── handler_sandbox.go       # create/get/list/pause/resume/terminate (no exec/files/shell here)
│   │   ├── handler_wallet.go
│   │   ├── handler_billing.go
│   │   ├── handler_user.go          # API key + credit endpoints
│   │   ├── handler_discovery.go
│   │   ├── middleware_logging.go
│   │   ├── middleware_recovery.go
│   │   └── router.go
│   ├── auth/                        # Native L402 + API key + JWT (control plane only)
│   │   ├── middleware.go
│   │   ├── l402.go
│   │   ├── apikey.go
│   │   ├── jwt.go
│   │   ├── ratelimit.go
│   │   └── data_jwt.go              # Mint and verify data-plane JWTs
│   ├── billing/                     # In-memory msat metering (control plane)
│   │   ├── engine.go                # BillingEngine: global ticker, worker pool
│   │   ├── balance.go               # Per-sandbox in-memory balance cache + meter
│   │   ├── topup.go                 # Threshold-triggered BOLT12 top-up flow
│   │   ├── audit.go                 # Batched 30s audit flush to Cloud SQL
│   │   └── recovery.go              # Per-instance startup recovery (owned shards only)
│   ├── orchestrator/                # Sandbox lifecycle (control plane)
│   │   ├── orchestrator.go
│   │   ├── host_pool.go             # In-memory host-VM membership; gRPC client pool
│   │   ├── router.go                # Tier → host-VM selection; standard tier routing
│   │   ├── gpu.go                   # On-demand GPU VM provisioning (GCP API)
│   │   └── snapshot.go              # Hot-snapshot orchestration (calls host-agent gRPC)
│   ├── cluster/                     # Cluster membership + consistent hashing (control plane)
│   │   ├── memberlist.go            # Hashicorp memberlist wrapper (port 7444)
│   │   ├── ring.go                  # Consistent-hash ring keyed on sandbox_id
│   │   ├── forwarder.go             # gRPC forwarding to the owning instance
│   │   └── lease.go                 # Optional Cloud SQL fallback lease for split-brain protection
│   ├── payment/                     # Lightning / L402 client
│   │   ├── payment.go
│   │   ├── lnd.go                   # LND gRPC client (HA failover)
│   │   ├── bolt12.go                # BOLT12 offer + invoice_request logic
│   │   ├── invoicepool.go           # Pre-minted L402 BOLT11 invoice pool
│   │   └── topup.go                 # TopUpBalance(agent, chunk_sats)
│   ├── store/                       # Cloud SQL adapter (used by both binaries)
│   │   ├── store.go                 # Store interface
│   │   ├── agents.go
│   │   ├── users.go
│   │   ├── sandboxes.go
│   │   ├── billing.go
│   │   ├── topups.go
│   │   ├── credits.go
│   │   ├── apikeys.go
│   │   ├── hosts.go
│   │   └── notify.go                # LISTEN/NOTIFY plumbing for cross-instance invalidation
│   ├── mcp/                         # MCP server (control plane)
│   │   ├── server.go                # MCP HTTP streaming transport
│   │   ├── session.go               # In-memory session store; affinity via consistent hash
│   │   └── tools.go                 # Tool definitions
│   ├── proto/                       # Generated gRPC stubs
│   │   ├── host_agent.proto         # Control-plane → host-agent control RPCs
│   │   └── cluster.proto            # Control-plane peer-to-peer forwarding RPCs
│   ├── hostagent/                   # Host-agent-only modules
│   │   ├── server.go                # gRPC :7443 + HTTPS :8443 servers
│   │   ├── firecracker.go           # Local Firecracker control via Unix socket
│   │   ├── snapshot.go              # Local SSD snapshot + async GCS upload
│   │   ├── vsock.go                 # vsock dial helper
│   │   ├── exec.go                  # /sandbox/{id}/exec handler
│   │   ├── files.go                 # /sandbox/{id}/files handler
│   │   ├── shell.go                 # /sandbox/{id}/shell WSS handler
│   │   ├── status.go                # /sandbox/{id}/status handler
│   │   ├── balance.go               # Local mirror of sandbox balance for billing fan-out
│   │   ├── billingfanout.go         # In-process channel fan-out to WSS clients
│   │   ├── heartbeat.go             # Periodic heartbeat to control plane
│   │   └── auth.go                  # Local data_jwt verification
│   └── config/
│       ├── config_control.go
│       └── config_host.go
├── migrations/                      # SQL migration files (Cloud SQL via dbmate)
├── Dockerfile.control-plane
├── Dockerfile.host-agent
├── cloudbuild.yaml
└── openapi.yaml                     # Auto-generated OpenAPI 3.0 spec for control-plane routes only
```

---

## Module Interfaces

Each module is depended on through an interface, not a concrete struct. This enables clean mocking in tests and clear cross-binary boundaries.

### Store Interface (used by both binaries)

**Agent operations:**
- `CreateAgent(ctx, bolt12Offer, jwtSub) → Agent`
- `GetAgentByJWT(ctx, jwtSub) → Agent`
- `GetAgentByOffer(ctx, bolt12Offer) → Agent`
- `IncrementAgentBalance(ctx, agentID, deltaMsats) → (newBalance int64, err error)`
- `DecrementAgentBalance(ctx, agentID, deltaMsats) → (newBalance int64, err error)`
- `GetAgentBalance(ctx, agentID) → int64`

**Sandbox operations:**
- `CreateSandbox(ctx, SandboxCreate) → Sandbox`
- `GetSandbox(ctx, sandboxID) → Sandbox`
- `GetSandboxesByAgent(ctx, agentID, filter) → []Sandbox`
- `GetSandboxesByOwnerInstance(ctx, instanceID) → []Sandbox` — used at startup for ownership recovery
- `UpdateSandboxStatus(ctx, sandboxID, status) → error`
- `UpdateSandboxHost(ctx, sandboxID, hostVMID, vsockCID) → error`
- `SetSandboxSnapshot(ctx, sandboxID, localPath, gcsPath) → error`
- `TerminateSandbox(ctx, sandboxID) → error`
- `IncrementSandboxConsumed(ctx, sandboxID, msats) → error`

**Billing operations:**
- `BatchInsertBillingIntervals(ctx, []BillingInterval) → error` — batched flush
- `GetBillingHistory(ctx, agentID, page, limit) → []BillingInterval`
- `GetTotalConsumed(ctx, sandboxID) → int64`

**Top-up operations:**
- `RecordTopUp(ctx, TopUp) → error`
- `GetTopUpHistory(ctx, agentID, page, limit) → []TopUp`

**Credit operations:**
- `DeductCredits(ctx, userID, sandboxID, apiKeyID, costCents, intervals []CreditInterval) → error`
- `GetUserCreditBalance(ctx, userID) → int64`
- `IncrementSandboxCredits(ctx, sandboxID, costCents) → error`

**API key operations:**
- `LookupAPIKey(ctx, keyHash) → APIKey`
- `RevokeAPIKey(ctx, apiKeyID) → error` — also issues `NOTIFY apikey_revoked`

**Host VM operations:**
- `UpsertHostVM(ctx, HostVM) → error`
- `UpdateHostHeartbeat(ctx, hostID) → error`
- `GetAllHosts(ctx) → []HostVM`
- `MarkHostUnreachable(ctx, hostID) → error`

### Orchestrator Interface (control plane)

- `CreateSandbox(ctx, CreateRequest) → Sandbox`
- `TerminateSandbox(ctx, sandboxID) → error`
- `PauseSandbox(ctx, sandboxID) → SnapshotResult`
- `ResumeSandbox(ctx, sandboxID) → ResumeResult`
- `RefreshHostPool(ctx) → error`

(`ExecCommand`, `WriteFile`, `ReadFile`, `GetSandboxMetrics` are removed from the control-plane orchestrator. They live on the host-agent and are reached directly by the SDK.)

### Billing Interface (control plane)

- `RegisterSandbox(ctx, sandbox, initialBalanceMsats) → error` — adds to in-memory cache
- `DeregisterSandbox(ctx, sandboxID) → SettleResult` — flushes pending audit row
- `GetSandboxBalance(sandboxID) → int64`
- `OnTopUpSettled(sandboxID, addedMsats) → error`
- `MigrateOwnership(sandboxIDs []string) → error` — called when ring rebalances

### Payment Interface

- `RegisterOffer(ctx, bolt12Offer) → AgentToken`
- `TopUpBalance(ctx, agentID, sats, idempotencyKey) → (preimage, error)` — single BOLT12 pull payment
- `RequestBOLT12Invoice(ctx, offer, sats, memo) → Invoice` — used by `TopUpBalance` internally
- `PayInvoice(ctx, invoice, idempotencyKey) → Preimage`
- `GetNodeBalance(ctx) → BalanceInfo`
- `CreateBOLT11Invoice(ctx, sats, memo, expirySecs) → BOLT11Invoice`
- `MintL402Invoice() → (BOLT11Invoice, preimage)` — pulls from the pre-minted pool
- `VerifyPreimage(preimage, paymentHash) → bool`

### Cluster Interface (control plane)

- `OwnerOf(sandboxID string) → InstanceID` — local consistent-hash lookup
- `IsOwner(sandboxID string) → bool`
- `Forward(ctx, sandboxID, req, resp) → error` — gRPC forward to owner
- `OnRingChange(callback func(added, removed []string))`
- `LocalInstanceID() → InstanceID`

### Host-Agent gRPC API (control plane → host-agent)

Defined in `internal/proto/host_agent.proto`:

- `CreateSandbox(SandboxSpec) returns (SandboxHandle)` — Firecracker pre-boot config + boot
- `PauseSandbox(SandboxID) returns (SnapshotHandle)` — local SSD snapshot
- `ResumeSandbox(SandboxID, SnapshotHandle) returns (Empty)`
- `TerminateSandbox(SandboxID) returns (Empty)`
- `MintDataJWT not used — the control plane mints; host-agent only verifies
- `UpdateBalance(SandboxID, NewBalanceMsats) returns (Empty)` — control plane pushes balance updates so the host-agent's WSS billing fan-out has accurate values
- `Heartbeat(HostStatus) returns (HostStatusAck)` — bidirectional heartbeat (host pushes capacity; CP responds with sandbox-ownership hints)

---

## Control-Plane Startup Sequence

`cmd/control-plane/main.go` wires modules in dependency order:

1. **Load config** from environment variables and GCP Secret Manager
2. **Initialize store** — pgx pool to Cloud SQL primary at `10.4.0.3:5432`; LISTEN/NOTIFY connection for cache invalidation
3. **Initialize cluster** — join memberlist on `:7444`; subscribe to ring-change events
4. **Initialize payment** — connect to active LND via `lnd-active` GCE label discovery; refill pre-minted invoice pool
5. **Initialize orchestrator** — load host-VM list from Cloud SQL; open persistent gRPC channels to each host-agent
6. **Initialize billing engine** — empty cache; ring callback wires sandbox migration
7. **Recover ownership** — for every sandbox where `cluster.IsOwner(sandbox_id)` returns true and `sandbox.status = 'running'`, hydrate the in-memory balance from `agents.balance_msats` (Lightning) or `users.credit_balance_usd_cents` (credits) and start the meter
8. **Initialize MCP server** — wire api interface; mount on the chi router
9. **Initialize HTTP API** — chi router with auth + logging + recovery middleware
10. **Start goroutines:**
    - Global billing ticker (one goroutine per binary, fans out work to a pool)
    - Audit flush goroutine (every 30s)
    - Host pool heartbeat refresher (every 30 seconds)
    - Cloud SQL `LISTEN` consumer
    - Memberlist event consumer
    - Graceful shutdown listener (SIGTERM)
11. **Start HTTP server** on `0.0.0.0:8081` (the internal LB terminates TLS in front)
12. **Start internal gRPC server** on `0.0.0.0:7444` (peer forwarding)

### Crash Recovery on Startup

On startup the new instance only takes ownership of sandboxes whose `hash(sandbox_id) % len(membership)` selects it. Other instances continue to own theirs. This avoids the duplicate-billing problem of every-instance recovery. If an instance dies and rejoins later, the ring rebalances and ownership transfers cleanly.

---

## Host-Agent Startup Sequence

`cmd/host-agent/main.go`:

1. Load config (own host_id from GCE metadata, control-plane peer list, mTLS certs)
2. Connect to Cloud SQL via private IP (read-only access — host-agent never writes the canonical balance, only reads)
3. Format and mount `/var/lib/sc/snapshots/` (local NVMe SSD) if not already
4. Inventory existing local snapshots; report to control plane on register
5. Start gRPC server on `:7443` with mTLS using the internal CA
6. Start HTTPS server on `:8443` with the per-host certificate (signed by the internal CA, recognized by `ilb-host-data`)
7. Open a streaming heartbeat to the control plane: `Heartbeat` bidirectional stream. Every 5s push `{cpu, mem_used, sandbox_count, fc_version}`; the control plane responds with the current set of sandboxes the host owns and any pending pause/terminate commands.
8. Start GCS uploader goroutine (looks for `pending_upload` snapshots on local SSD older than 5 min idle)

---

## HTTP Router — All Routes (Control Plane)

The control plane only mounts routes that need a control-plane decision. All sandbox data-plane routes live on the host-agent (separate router on `:8443`).

### Middleware Stack (applied to all `/v1/` routes on the control plane)

1. `RequestIDMiddleware` — generates or propagates `X-Request-ID`
2. `StructuredLoggingMiddleware`
3. `RecoveryMiddleware`
4. `AuthMiddleware` — single-pass dispatch by `Authorization` scheme; sets `Principal` on context
5. `RateLimitMiddleware` — per-Principal token bucket
6. `OwnershipForwardMiddleware` — for routes parameterized by `{sandbox_id}`, if `!cluster.IsOwner(id)` then `cluster.Forward` and short-circuit
7. `ContentTypeMiddleware` — enforces application/json for POST/PUT bodies

### Control-Plane Route Table

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/healthz` | `HandleHealthCheck` | Public |
| `GET` | `/metrics` | `HandlePrometheusMetrics` | Public (Cloud-internal only via VPC firewall) |
| `GET` | `/v1/pricing` | `HandleGetPricing` | Public |
| `GET` | `/v1/templates` | `HandleListTemplates` | Public |
| `POST` | `/v1/wallet/offer` | `HandleRegisterOffer` | L402 (1 sat) |
| `GET` | `/v1/wallet/balance` | `HandleGetBalance` | L402 + JWT |
| `POST` | `/v1/wallet/topup` | `HandleManualTopup` | L402 + JWT |
| `POST` | `/v1/sandbox/create` | `HandleCreateSandbox` | L402 or API key |
| `GET` | `/v1/sandbox/{id}` | `HandleGetSandbox` | L402 or API key |
| `POST` | `/v1/sandbox/{id}/pause` | `HandlePause` | L402 or API key + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/resume` | `HandleResume` | L402 or API key + sandbox_id caveat |
| `DELETE` | `/v1/sandbox/{id}` | `HandleTerminate` | L402 or API key + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/refresh-data-jwt` | `HandleRefreshDataJWT` | L402 or API key + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/extend-balance` | `HandleExtendBalance` | L402 or API key + sandbox_id caveat |
| `POST` | `/v1/sandbox/{id}/delegate` | `HandleDelegate` | L402 or API key + sandbox_id caveat |
| `GET` | `/v1/billing/history` | `HandleBillingHistory` | L402 or API key |
| `GET` | `/v1/sandbox/{id}/billing` | `HandleSandboxBilling` | L402 or API key + sandbox_id caveat |
| `GET` | `/v1/mcp/stream` | `MCPStreamHandler` | JWT or API key |
| `POST` | `/v1/user/api-keys` | `HandleCreateAPIKey` | User JWT |
| `GET` | `/v1/user/api-keys` | `HandleListAPIKeys` | User JWT |
| `PATCH` | `/v1/user/api-keys/{id}` | `HandleUpdateAPIKey` | User JWT |
| `DELETE` | `/v1/user/api-keys/{id}` | `HandleRevokeAPIKey` | User JWT |
| `GET` | `/v1/user/profile` | `HandleUserProfile` | User JWT |
| `GET` | `/v1/user/credits` | `HandleGetCredits` | User JWT |
| `GET` | `/v1/user/credits/history` | `HandleGetCreditsHistory` | User JWT |
| `POST` | `/internal/webhooks/stripe` | `HandleStripeWebhook` | Stripe signature |

### Host-Agent Route Table (HTTPS :8443)

The host-agent has a much smaller surface. Routes are reachable through the global LB at `/h/{host_id}/...`; the internal LB strips the prefix.

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/healthz` | `HostHealth` | Public (LB only via firewall) |
| `POST` | `/sandbox/{id}/exec` | `HostExec` | data_jwt with `exec` scope |
| `POST` | `/sandbox/{id}/files` | `HostWriteFile` | data_jwt with `files` scope |
| `GET` | `/sandbox/{id}/files` | `HostReadFile` | data_jwt with `files` scope |
| `GET` | `/sandbox/{id}/files/list` | `HostListFiles` | data_jwt with `files` scope |
| `GET` | `/sandbox/{id}/status` | `HostStatus` | data_jwt with `status` scope |
| `GET` | `/sandbox/{id}/shell` | `HostShellWSS` | data_jwt with `shell` scope (token via query param) |

---

## Handler Specifications

### POST /v1/wallet/offer — HandleRegisterOffer

Unchanged in spec. Validates the BOLT12 offer locally, creates an `agents` row with `balance_msats = 0`, mints a Supabase JWT, and returns it.

### POST /v1/sandbox/create — HandleCreateSandbox

**Purpose:** Provision a new sandbox environment. Triggers VM creation on a chosen host, opens an initial prepaid balance, and registers the sandbox with the billing engine.

**Request body:**
- `tier` (string, required) — `nano | micro | standard | gpu_small | gpu_large`
- `template` (string, optional) — `python3 | nodejs | rust | ubuntu-base | ml-cuda`
- `duration_secs` (integer, optional) — initial balance hint; clamped to `[60, 14_400]`
- `egress` (boolean, optional, default false) — enable outbound internet
- `label` (string, optional)
- `idempotency_key` (string, required) — caller-provided UUID; duplicate calls return the same sandbox

**Validation:**
- `tier` must be a known value
- For L402: agent's BOLT12 offer must be reachable (a probe `invoice_request` for 1 sat is sent if the agent has zero on-record balance)
- For API key: user must have non-zero `credit_balance_usd_cents`
- Concurrent sandbox limit not exceeded

**Processing:**
1. Compute `initial_top_up_sats = max(60, duration_secs) * tier.sats_per_second`
2. Create sandbox row in Cloud SQL with `status = 'pending'`, `owner_instance_id = cluster.LocalInstanceID()`
3. Call `orchestrator.CreateSandbox(tier, template, sandbox_id)`:
   - Pick a host VM via `host_pool.PickFirecrackerHost(tier)` or `host_pool.ProvisionGPUHost(tier)`
   - Issue gRPC `CreateSandbox` to host-agent on the chosen host
   - Receive `{vsock_cid, fc_pid}` back
4. For L402 path: call `payment.TopUpBalance(agent_id, initial_top_up_sats, idempotency_key)` — pulls funds from agent's BOLT12 offer; on failure, terminate the sandbox and return 402
5. For API key path: deduct `initial_top_up_cents` from `users.credit_balance_usd_cents` atomically
6. Update sandbox row: `status = 'running'`, `host_vm_id`, `vsock_cid`
7. Mint `data_jwt` scoped to this sandbox (1h expiry, scopes: `exec`, `files`, `shell`, `status`)
8. Call `billing.RegisterSandbox(sandbox, initial_balance_msats)` — adds to in-memory cache and starts metering
9. Return the response below

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "running",
  "tier": "micro",
  "runtime": "firecracker",
  "template": "python3",
  "data_endpoint": "https://api.sockt.io/h/vm-host-pool-001/sandbox/{id}",
  "data_jwt": "eyJ...",
  "data_jwt_expires_at": "ISO8601",
  "shell_url": "wss://api.sockt.io/h/vm-host-pool-001/sandbox/{id}/shell",
  "sats_per_second": 2.0,
  "initial_balance_sats": 600,
  "host_vm": "vm-host-pool-001",
  "created_at": "ISO8601",
  "estimated_ready_at": "ISO8601"
}
```

**Error cases:**
- `400 invalid_tier`
- `400 invalid_template`
- `402 topup_failed` — agent's BOLT12 offer rejected the initial pull (Lightning routing failure or offer-balance reasons)
- `402 insufficient_credits` — API-key user lacks credits
- `429 sandbox_limit_exceeded`
- `503 no_capacity`

### POST /v1/sandbox/{id}/pause — HandlePause

**Processing:**
1. `OwnershipForwardMiddleware` ensures we're the owner; otherwise forward
2. Call `billing.DeregisterSandbox(sandbox_id)` — settles partial interval, flushes audit row, removes from in-memory cache
3. Call `orchestrator.PauseSandbox(sandbox_id)` — issues gRPC `PauseSandbox` to host-agent; host writes snapshot to local SSD and (synchronously) returns `local_snapshot_path`. GCS upload is queued for later.
4. Update sandbox row: `status = 'paused'`, `local_snapshot_path`, `snapshot_node_host_id` (so we know which host's local SSD has the hot copy)

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "paused",
  "paused_at": "ISO8601",
  "snapshot_size_mb": 256,
  "msats_consumed_total": 6_840_000,
  "snapshot_expires_at": "ISO8601",
  "snapshot_storage": "host_local_ssd"
}
```

### POST /v1/sandbox/{id}/resume — HandleResume

**Processing:**
1. Verify `status = 'paused'`
2. Try to resume on the original host (`snapshot_node_host_id`) — if reachable and has the snapshot on local SSD, hot resume in < 1s
3. If not, download from GCS (where the host-agent's GCS uploader put it) to a fresh host's local SSD, then resume
4. Issue gRPC `ResumeSandbox` to the (possibly new) host-agent
5. Update sandbox row: new `host_vm_id` (if migrated), new `vsock_cid`
6. Mint a fresh `data_jwt` (scopes unchanged; new `host_id` may differ)
7. Re-register with the billing engine on the *new* owner instance (consistent hash may now point elsewhere if ring changed — handled transparently)

**Response 200:**
```
{
  "sandbox_id": "uuid",
  "status": "running",
  "data_endpoint": "https://api.sockt.io/h/{new_host_id}/sandbox/{id}",
  "data_jwt": "eyJ...",
  "shell_url": "wss://api.sockt.io/h/{new_host_id}/sandbox/{id}/shell",
  "resumed_at": "ISO8601",
  "resumed_from": "host_local_ssd | gcs"
}
```

### DELETE /v1/sandbox/{id} — HandleTerminate

1. `billing.DeregisterSandbox` — final settle + audit flush
2. `orchestrator.TerminateSandbox` — gRPC `TerminateSandbox` to host-agent
3. Update sandbox row: `status = 'terminated'`, `terminated_at`
4. Schedule GCS snapshot delete (lifecycle rules also handle this)

### POST /v1/sandbox/{id}/refresh-data-jwt

Re-mints `data_jwt` for an existing sandbox. Returns the new JWT and the current `data_endpoint` (in case the host changed since the last JWT was issued, e.g., after resume on a different host).

### POST /v1/sandbox/{id}/extend-balance

Manual trigger for the agent to pre-fund a known-large workload. The control plane calls `payment.TopUpBalance(agent_id, sats, idempotency_key)`; on success, increments the in-memory balance and pushes the new value to the host-agent via gRPC `UpdateBalance`.

### Other handlers

`HandleGetSandbox`, `HandleBillingHistory`, `HandleSandboxBilling`, `HandleDelegate` are routed but otherwise unchanged from v1 in shape.

`HandleExec`, `HandleWriteFile`, `HandleReadFile`, `HandleGetSandboxStatus`, `WSSUpgradeHandler` are **removed from the control-plane router** — they live on the host-agent now.

---

## Host-Agent Handler Sketches (HTTPS :8443)

### POST /sandbox/{id}/exec — HostExec

1. Verify `data_jwt` (constant-time HMAC against the shared secret) — scope must include `exec`
2. Look up local sandbox by `sandbox_id` — must be in the host-agent's own sandbox table
3. Open a vsock connection to the guest agent (or local exec for GPU VMs); send the exec command
4. Stream output back as a JSON response (or buffered if `stream=false`)
5. Update local balance cache: subtract no msats (the meter already counted; this is just for status visibility)

**Response 200:**
```
{ "stdout": "...", "stderr": "...", "exit_code": 0, "duration_ms": 142, "timed_out": false }
```

### POST /sandbox/{id}/files — HostWriteFile

Verify `data_jwt`, dial vsock, write file via guest agent's `write_file` op.

### GET /sandbox/{id}/files — HostReadFile

Verify `data_jwt`, dial vsock, read file via guest agent.

### GET /sandbox/{id}/status — HostStatus

Returns local cached metrics and balance:
```
{
  "sandbox_id": "...", "status": "running", "tier": "micro",
  "uptime_secs": 342, "balance_msats": 4_320_000,
  "consumed_msats_lifetime": 6_840_000,
  "cpu_percent": 12.4, "memory_used_mb": 312, "memory_limit_mb": 1024,
  "host_vm": "vm-host-pool-001", "template": "python3",
  "created_at": "ISO8601", "last_metered_at": "ISO8601"
}
```

### GET /sandbox/{id}/shell — HostShellWSS

WSS upgrade. After upgrade: spawn vsock proxy goroutine + billing fan-out goroutine. See Component 8 for the full message protocol.

---

## Configuration Reference

### Control-Plane Variables

| Variable | Description | Source |
|---|---|---|
| `CLOUDSQL_HOST` | Cloud SQL private IP | env (`10.4.0.3`) |
| `CLOUDSQL_PASSWORD` | Cloud SQL password | Secret Manager |
| `CLOUDSQL_DB` | DB name | env (`sc`) |
| `CLOUDSQL_USER` | DB user | env (`control_plane`) |
| `SUPABASE_JWT_SECRET` | JWT verify-only key | Secret Manager |
| `LND_GRPC_HOST_PRIMARY` | Primary LND gRPC | env (`10.0.0.5:10009`) |
| `LND_GRPC_HOST_SECONDARY` | Secondary LND gRPC | env (`10.0.0.6:10009`) |
| `LND_MACAROON_HEX` | LND invoice macaroon | Secret Manager |
| `LND_TLS_CERT_B64` | LND TLS cert | Secret Manager |
| `WSS_JWT_SECRET` | HMAC secret for `data_jwt` and `wss_token` | Secret Manager |
| `HOST_AGENT_MTLS_CA_CERT` | Internal CA for mTLS to host-agents | Secret Manager |
| `HOST_AGENT_MTLS_CLIENT_CERT` | Per-instance client cert | Secret Manager |
| `HOST_AGENT_MTLS_CLIENT_KEY` | Per-instance client key | Secret Manager |
| `KMS_SSH_KEY_RESOURCE` | KMS key resource for SSH DEK wrapping | env |
| `GCP_PROJECT_ID` | env | |
| `GCP_REGION` | env | |
| `GCS_SNAPSHOT_BUCKET` | env (`sc-snapshots`) | |
| `BILLING_TICK_MS` | Default 1000 (1 sec) | env |
| `BILLING_AUDIT_FLUSH_SECS` | Default 30 | env |
| `BALANCE_LOW_WATER_SECS` | Top-up trigger threshold (default 60) | env |
| `BALANCE_TOPUP_CHUNK_SECS` | Top-up size in seconds of runtime (default 300) | env |
| `INVOICE_POOL_SIZE` | Pre-minted invoice pool target (default 1000) | env |
| `MEMBERLIST_PORT` | Cluster gossip port (default 7444) | env |
| `MAX_EXEC_TIMEOUT_MS` | Default 300000 | env |
| `LOG_LEVEL` | env | |

### Host-Agent Variables

| Variable | Description | Source |
|---|---|---|
| `HOST_ID` | GCE instance name | metadata server |
| `CONTROL_PLANE_PEERS` | List of control-plane internal IPs:7444 | env (refreshed via metadata) |
| `MTLS_SERVER_CERT` | host-agent server cert (signed by internal CA) | Secret Manager |
| `MTLS_SERVER_KEY` | host-agent server key | Secret Manager |
| `WSS_JWT_SECRET` | Same secret as control plane (verify-only here) | Secret Manager |
| `CLOUDSQL_HOST_RO` | Cloud SQL read-only endpoint | env |
| `LOCAL_SNAPSHOT_DIR` | `/var/lib/sc/snapshots/` | env |
| `GCS_SNAPSHOT_BUCKET` | env | |
| `SNAPSHOT_GCS_UPLOAD_IDLE_MIN` | Default 5 | env |
| `FIRECRACKER_BIN` | `/usr/bin/firecracker` | env |
| `LOG_LEVEL` | env | |

---

## Graceful Shutdown

### Control plane (SIGTERM, 60s drain configured by MIG)

1. Stop accepting new HTTP connections on `:8081`
2. Inform memberlist of imminent leave; let the ring rebalance (other instances pick up sandboxes)
3. For sandboxes still owned by this instance during the drain, push their final state (`balance_msats`) to Cloud SQL via `billing.audit.FlushAll`
4. Wait for in-flight HTTP requests to complete (timeout: 30s)
5. Close pgx pool, LND gRPC, host-agent gRPC channels
6. Exit 0

### Host-agent (SIGTERM)

1. Stop accepting new HTTPS data-plane connections
2. Send a `host_draining` event on the heartbeat stream so the control plane stops scheduling new sandboxes here
3. Wait for in-flight exec/file calls to complete (timeout: 30s)
4. WSS shell connections receive `{"type":"host_draining"}` and are asked to reconnect; they re-resolve `data_endpoint` via `/v1/sandbox/{id}/refresh-data-jwt`
5. Persist any in-flight balance updates to Cloud SQL via the control plane (host-agent never writes balance directly)
6. Exit 0

---

## Observability

### Metrics (Prometheus, exported to Cloud Monitoring)

Control plane:

| Metric | Type | Labels |
|---|---|---|
| `cp_http_requests_total` | Counter | method, path, status |
| `cp_http_request_duration_ms` | Histogram | method, path |
| `cp_active_sandboxes` | Gauge | tier, runtime |
| `cp_owned_sandboxes` | Gauge | — (per instance) |
| `cp_ring_rebalance_events_total` | Counter | reason (member_join/member_leave) |
| `cp_peer_forward_total` | Counter | direction |
| `cp_billing_meter_tick_duration_us` | Histogram | — |
| `cp_billing_topup_total` | Counter | result (ok/fail) |
| `cp_billing_audit_flush_rows` | Histogram | — |
| `cp_db_query_duration_ms` | Histogram | operation |

Host agent:

| Metric | Type | Labels |
|---|---|---|
| `ha_active_sandboxes` | Gauge | — |
| `ha_exec_duration_ms` | Histogram | tier |
| `ha_file_op_duration_ms` | Histogram | op |
| `ha_wss_connections_active` | Gauge | — |
| `ha_vsock_dial_duration_us` | Histogram | — |
| `ha_snapshot_write_duration_ms` | Histogram | — |
| `ha_snapshot_gcs_upload_duration_ms` | Histogram | — |

### Structured Log Fields

Every request log entry includes:
- `request_id`, `agent_id` or `user_id`, `sandbox_id` (where applicable)
- `method`, `path`, `status_code`, `duration_ms`
- `tier`, `runtime`, `host_id` (for sandbox operations)
- `instance_id` (control plane: which instance handled or owned)
- `error` (structured slug, never stack trace in production)
