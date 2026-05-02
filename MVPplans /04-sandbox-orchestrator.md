# Component 4: Sandbox Orchestrator (RunPod MVP)

## v3 changelog

- Firecracker lifecycle orchestration removed from MVP.
- Host-pool capacity model and MIG autoscaling removed.
- `host_agent.proto` control protocol removed from MVP path.
- Orchestration now uses RunPod API for create/stop/resume/terminate.
- Pause/resume durability moved to RunPod network volumes.
- Control-plane no longer manages per-host heartbeats.

---

## Overview

The orchestrator is the `internal/orchestrator` module in the Cloud Run control plane. It manages sandbox lifecycle by calling RunPod APIs and persisting state in Supabase.

Runtime lifecycle is now:

- create sandbox -> deploy RunPod pod
- pause sandbox -> stop RunPod pod
- resume sandbox -> resume same pod or redeploy with same network volume
- terminate sandbox -> terminate pod and delete volume

---

## Runtime Decision Matrix

| Tier | Runtime | RunPod type | Notes |
|---|---|---|---|
| nano | runpod_cpu | Community CPU pod | Cheapest tier |
| micro | runpod_cpu | Community CPU pod | Default dev workloads |
| standard | runpod_cpu | Higher vCPU/RAM CPU pod | Heavy CPU workloads |
| gpu_small | runpod_gpu | T4-class pod | Starter GPU |
| gpu_large | runpod_gpu | A100-class pod | High-memory GPU |

---

## Module Structure

### Orchestrator

Fields:

- `Store` (Supabase-backed)
- `RunPodClient` (`internal/orchestrator/runpod.go`)
- `PricingCache`
- `JWTSigner` for `data_jwt`
- `Config` (RunPod template IDs, regions, defaults)

### RunPodClient

Core methods:

- `CreatePod(ctx, spec) -> PodInfo`
- `StopPod(ctx, podID) -> error`
- `ResumePod(ctx, podID) -> PodInfo`
- `TerminatePod(ctx, podID) -> error`
- `CreateNetworkVolume(ctx, sizeGB) -> VolumeInfo`
- `DeleteNetworkVolume(ctx, volumeID) -> error`
- `GetPod(ctx, podID) -> PodInfo`

---

## RunPod API Contract (MVP)

The orchestrator wraps RunPod GraphQL/REST operations:

- `podFindAndDeployOnDemand`
- `podStop`
- `podResume`
- `podTerminate`
- `networkVolumeCreate`
- `networkVolumeDelete`

All responses are normalized into internal structs so higher layers do not depend on RunPod schema details.

---

## Sandbox Lifecycle

### Create

1. Validate tier/template/auth/balance.
2. Create sandbox row with `status='pending'`.
3. Create network volume (optional by tier/template policy).
4. Deploy RunPod pod with:
   - selected template image
   - env vars (`SANDBOX_ID`, callback URL, JWT secret reference)
   - exposed ports (`8443` for HTTPS/WSS runtime)
5. Persist `pod_id`, `pod_proxy_url`, `network_volume_id`.
6. Mint `data_jwt` with `pod_id` claim.
7. Mark sandbox `running`.

### Pause

1. Flush pending billing rows from runtime callback path.
2. Call `podStop(pod_id)`.
3. Mark sandbox `paused`.
4. Keep `network_volume_id` for resume.

### Resume

1. Ensure sandbox is `paused`.
2. Attempt `podResume(pod_id)`.
3. If resume not available, deploy a new pod with the same volume.
4. Update `pod_id` and `pod_proxy_url` if changed.
5. Mint fresh `data_jwt`.
6. Mark sandbox `running`.

### Terminate

1. Call `podTerminate(pod_id)`.
2. Delete network volume if retention policy is `delete_on_terminate`.
3. Mark sandbox `terminated`.
4. Revoke existing `data_jwt`.

---

## Data Plane Endpoints

The control plane returns:

- `data_endpoint = https://<pod_id>-8443.proxy.runpod.net/sandbox/{sandbox_id}`
- `shell_url = wss://<pod_id>-8443.proxy.runpod.net/sandbox/{sandbox_id}/shell`

The runtime pod (`sockt-runtime`) serves:

- `POST /sandbox/{id}/exec`
- `POST /sandbox/{id}/files`
- `GET /sandbox/{id}/files`
- `GET /sandbox/{id}/files/list`
- `GET /sandbox/{id}/status`
- `GET /sandbox/{id}/shell` (WSS)

---

## Supabase State Fields

`sandboxes` additions used by orchestrator:

- `runtime` (`runpod_cpu | runpod_gpu`)
- `pod_id`
- `pod_proxy_url`
- `runpod_region`
- `network_volume_id`
- `last_resume_from` (`resume | redeploy`)

---

## Capacity and Scheduling

MVP scheduling strategy:

- region allowlist (start with one or two regions)
- best-effort deployment on requested tier
- fail fast on no-capacity and return actionable error to client

No local bin-packing or host balancing is needed in MVP.

---

## Security Model (MVP)

- Runtime pods authenticate requests via `data_jwt`.
- `data_jwt` includes `sandbox_id`, `pod_id`, scopes, expiration.
- Runtime must reject token when `pod_id` claim mismatches local pod identity.
- Secrets are injected from image-time or startup-time env, never hardcoded.

Community cloud vs secure cloud:

- Community cloud is default for cost.
- Secure cloud can be used per tenant/tier when stronger isolation/SLA is required.

---

## Failure Handling

| Failure | Behavior |
|---|---|
| RunPod API timeout | retry with exponential backoff, then return 503 |
| Pod launch failed | mark sandbox `failed_create` and return `no_capacity` |
| Pod not reachable after create | terminate pod, retry once |
| Resume failed | fallback to redeploy using same volume |
| Volume attach failed | terminate pod, keep sandbox paused, alert |

---

## Metrics

| Metric | Type |
|---|---|
| `orch_runpod_create_duration_ms` | Histogram |
| `orch_runpod_pause_duration_ms` | Histogram |
| `orch_runpod_resume_duration_ms` | Histogram |
| `orch_runpod_terminate_duration_ms` | Histogram |
| `orch_runpod_failures_total` | Counter by operation/reason |
| `orch_active_sandboxes` | Gauge by tier/runtime |

---

## Dependencies

- Supabase backend (Component 6)
- Payment module (Component 5)
- Billing engine callbacks (Component 3)
- Runtime pod API and WSS protocol (Component 8)

---

## Phase 2 (post-MVP)

Restore v2 host-based architecture if needed:

- Firecracker lifecycle control via host-agent
- host pool heartbeats and autoscaling
- hot local SSD snapshots + cold object storage
- dedicated gRPC control mesh
