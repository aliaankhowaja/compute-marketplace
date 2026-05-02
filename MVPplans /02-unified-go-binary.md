# Component 2: Control Plane + Runtime Binary (MVP)

## v3 changelog

- Removed `internal/cluster/` and all consistent-hash ownership logic.
- Removed peer forwarding and memberlist dependencies.
- Replaced `cmd/host-agent/` with `cmd/sockt-runtime/` for RunPod pods.
- Added `internal/runtime/` package for in-pod data plane + metering.
- Replaced host-pool gRPC mesh with RunPod API orchestration.
- Control plane startup simplified for Cloud Run stateless execution.

---

## Overview

The codebase now builds two binaries from one repo:

1. `cmd/control-plane/` - Cloud Run service handling auth, lifecycle APIs, payment, MCP.
2. `cmd/sockt-runtime/` - in-pod runtime running inside each RunPod sandbox pod.

The control plane is no longer state-sharded across instances; the runtime pod owns short-lived per-sandbox hot state.

---

## Repository Structure

```
sockt/
├── cmd/
│   ├── control-plane/
│   │   └── main.go
│   └── sockt-runtime/
│       └── main.go
├── internal/
│   ├── api/
│   ├── auth/
│   ├── billing/
│   ├── orchestrator/
│   │   ├── orchestrator.go
│   │   └── runpod.go
│   ├── payment/
│   ├── store/
│   ├── mcp/
│   ├── runtime/
│   │   ├── server.go
│   │   ├── exec.go
│   │   ├── files.go
│   │   ├── shell.go
│   │   ├── status.go
│   │   ├── meter.go
│   │   ├── callbacks.go
│   │   └── auth.go
│   └── config/
```

Removed: `internal/cluster/`, `internal/proto/host_agent.proto`, host-peer forwarding stack.

---

## Interface Changes

### Orchestrator interface

- `CreateSandbox`
- `PauseSandbox`
- `ResumeSandbox`
- `TerminateSandbox`
- `GetRuntimeEndpoint`

Removed:

- host gRPC client pool methods
- cluster ownership forwarding APIs

### Billing interface

Control plane billing now focuses on:

- accepting periodic audit batches from runtime pods
- processing top-up triggers
- writing durable balances

No per-second global ticker in control plane.

---

## Control Plane Startup (Cloud Run)

1. Load config + secrets.
2. Init Supabase DB/pool.
3. Init payment module (single LND connection).
4. Init RunPod orchestrator client.
5. Init billing ingestion + audit batch writer.
6. Init API routes + MCP routes.
7. Start HTTP server (`:8080` in container).

No memberlist join, no ring rebalance listeners, no peer gRPC server.

---

## Runtime Startup (RunPod pod)

1. Load sandbox/pod metadata from env.
2. Start HTTPS server for data-plane operations (`:8443`).
3. Start WSS shell endpoint.
4. Start local metering ticker (`1s`) for this sandbox.
5. Start callback worker posting billing/top-up/pause signals to control plane.

The runtime binary is intentionally small and single-sandbox-scoped.

---

## HTTP Routes

### Control plane routes

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/sandbox/create` | create RunPod sandbox |
| `POST` | `/v1/sandbox/{id}/pause` | pause by stopping pod |
| `POST` | `/v1/sandbox/{id}/resume` | resume or redeploy pod |
| `DELETE` | `/v1/sandbox/{id}` | terminate pod |
| `POST` | `/v1/sandbox/{id}/refresh-data-jwt` | renew pod-scoped JWT |
| `POST` | `/v1/runtime/billing/audit-batch` | runtime callback for 30s billing rows |
| `POST` | `/v1/runtime/topup-trigger` | runtime callback for low-balance top-up |
| `POST` | `/v1/runtime/pause-trigger` | runtime callback for out-of-balance pause |

### Runtime routes

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/sandbox/{id}/exec` | command execution |
| `POST` | `/sandbox/{id}/files` | write file |
| `GET` | `/sandbox/{id}/files` | read file |
| `GET` | `/sandbox/{id}/files/list` | list files |
| `GET` | `/sandbox/{id}/status` | runtime status |
| `GET` | `/sandbox/{id}/shell` | websocket shell |

---

## Auth Responsibilities

- Control plane validates L402 / API key / user JWT.
- Control plane mints `data_jwt` with claims: `sandbox_id`, `pod_id`, `scope`, `exp`.
- Runtime validates `data_jwt` and enforces:
  - claim sandbox match
  - claim pod match
  - scope restrictions

---

## Error Handling Changes

New canonical transient runtime errors:

- `pod_unreachable`
- `pod_starting`
- `pod_replaced`
- `runpod_capacity_unavailable`

Removed error classes tied to cluster forwarding and host-agent mesh failure.

---

## Observability

Control plane metrics:

- API latency/error rates
- RunPod orchestration latency
- payment/top-up outcomes
- audit batch ingestion

Runtime metrics:

- exec/file/shell latency
- local meter tick health
- callback success/failure
- websocket active sessions

---

## Phase 2 (post-MVP)

Reintroduce v2 modules only when needed:

- `internal/cluster/` for sharded ownership
- host-based runtime fabric
- internal gRPC control mesh
- centralized control-plane meter
