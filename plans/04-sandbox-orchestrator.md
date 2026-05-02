# Component 4: Sandbox Orchestrator

## v2 changelog

- All Firecracker control moved off the control plane. The control plane no longer talks to a Firecracker Unix socket directly; instead it issues mTLS gRPC calls to the `host-agent` binary running on each host VM (Component 2). This eliminates SSH port-forwarding, makes host control sub-1ms in-VPC, and removes the need for inbound SSH from the control plane to host VMs.
- The host-agent owns the entire data plane (`exec`, `read_file`, `write_file`, `shell`) — terminating directly on the host that runs the sandbox. Clients reach it through the global LB at `/h/{host_id}/sandbox/{id}/...` via `ilb-host-data` (Component 9).
- Snapshots are now written to **host-local NVMe SSD** (`/var/lib/sc/snapshots/`) for sub-second hot resume on the same host. A background goroutine on the host-agent uploads to GCS asynchronously after 5 min of pause idle (cold tier).
- GPU VMs run the same host-agent binary. SSH-per-exec is removed; the host-agent talks to the local sandbox process group via standard OS APIs. SSH remains as a fallback only.
- SSH key encryption (when used) now uses a per-sandbox DEK wrapped by GCP KMS (`projects/.../keyRings/sc/cryptoKeys/sandbox-ssh-key`), not a key derived from the BOLT12 offer hash.
- Host pool autoscaling moved from the orchestrator's MIG-direct calls to the `mig-fc-hosts` MIG with a custom-metric autoscaler driven by the orchestrator's published `host_pool_demand_pct`.

---

## Overview

The Sandbox Orchestrator is the `internal/orchestrator` module within the **control-plane binary**. It is responsible for the complete lifecycle of every compute environment — provisioning, monitoring, snapshotting, restoring, and terminating — by issuing gRPC commands to the right host-agent.

The orchestrator manages two runtime backends through one uniform gRPC API:

- **Firecracker microVMs** (nano, micro, optionally standard) on `mig-fc-hosts` host VMs.
- **Dedicated GCP Compute Engine VMs** (standard overflow, GPU tiers) created on demand.

Both kinds of host run the same `host-agent` binary, so the orchestrator's interface to either is identical: `host_agent.CreateSandbox(spec)`, `host_agent.PauseSandbox(id)`, etc. The difference is only that the host-agent on a Firecracker host launches a microVM and the host-agent on a GPU VM launches a process group on the local OS.

The data plane (`exec`, `files`, `shell`) is **not** part of the orchestrator. It lives entirely on the host-agent and is reached directly by the SDK after sandbox creation.

---

## Runtime Decision Matrix

| Tier | vCPU | RAM | Primary Runtime | Fallback Runtime | Host Type |
|---|---|---|---|---|---|
| nano | 0.25 (shared) | 256 MB | Firecracker microVM | None | GCP c2-standard-16 (nested KVM) |
| micro | 1 | 1 GB | Firecracker microVM | None | GCP c2-standard-16 (nested KVM) |
| standard | 2–4 | 4–8 GB | Firecracker microVM | Dedicated n2-standard-4 | c2-standard-16 OR n2-standard-4 |
| gpu_small | 4 + T4 GPU | 16 GB | Dedicated VM | None | GCP n1-standard-8 + NVIDIA T4 |
| gpu_large | 8 + A100 | 40–80 GB | Dedicated VM | None | GCP a2-highgpu-1g |

### Standard Tier Routing Logic

The orchestrator checks the Firecracker host pool's available capacity first. If any host VM has at least 4GB free RAM:
- Route to Firecracker (fast, < 500ms cold start)

If no Firecracker host has capacity:
- Provision a dedicated `n2-standard-4` GCP VM (30–60s cold start)
- Notify caller via `estimated_ready_at` in the response

---

## Module Structure

### Orchestrator (control plane)

The top-level struct. Holds:
- `Store` interface reference
- `HostPool` — in-memory map of host_id → `HostAgentClient` (persistent gRPC channel)
- `GCPComputeClient` — Compute Engine API client (`cloud.google.com/go/compute`)
- `GCSClient` — Google Cloud Storage client for cold snapshot tier
- `KMSClient` — GCP KMS client for SSH DEK wrapping
- Config: `GCPProject`, `GCPZone`, `SnapshotBucket`, `VPCNetwork`, `VPCSubnet`, `KMSSSHKeyResource`

### HostPool (control plane)

Maintains an in-memory map of host VMs and their current capacity. Synchronized with Cloud SQL `host_vms` via the bidirectional gRPC heartbeat stream that each host-agent opens at startup.

Fields per host entry:
- `hostID` — GCE instance name
- `internalIP` — VPC IP (for gRPC dial)
- `kind` — `firecracker | gpu_small | gpu_large | dedicated_standard`
- `totalMemMB`, `allocatedMemMB`
- `firecrackerCount`
- `status` — `ready | draining | unreachable`
- `lastHeartbeatAt`
- `client` — persistent `HostAgentClient` (mTLS gRPC channel; auto-reconnect)

### HostAgentClient (control plane)

A thin wrapper around the generated gRPC stub from `internal/proto/host_agent.proto`. Persistent HTTP/2 connection with mTLS, gRPC keepalives every 15s, automatic reconnect on transport errors. One client per host.

---

## host-agent gRPC API (the control protocol)

Defined in `internal/proto/host_agent.proto`. mTLS server cert per host signed by the internal CA stored in Secret Manager.

```proto
service HostAgent {
  rpc CreateSandbox(SandboxSpec) returns (SandboxHandle);
  rpc PauseSandbox(SandboxRef) returns (SnapshotMetadata);
  rpc ResumeSandbox(ResumeSpec) returns (SandboxHandle);
  rpc TerminateSandbox(SandboxRef) returns (google.protobuf.Empty);
  rpc UpdateBalance(BalanceUpdate) returns (google.protobuf.Empty);
  rpc GetSandboxMetrics(SandboxRef) returns (SandboxMetrics);
  rpc Heartbeat(stream HostStatus) returns (stream HostCommand);
  rpc PrepareSnapshotForMigration(SandboxRef) returns (MigrationHandle);
  rpc LoadSnapshotFromGCS(GCSSnapshotRef) returns (google.protobuf.Empty);
}

message SandboxSpec {
  string sandbox_id = 1;
  string tier = 2;
  string template_id = 3;
  uint32 mem_mb = 4;
  uint32 vcpu = 5;
  bool egress_enabled = 6;
  bytes ssh_pubkey = 7;             // for GPU/dedicated VMs only
  bytes wrapped_ssh_dek = 8;        // KMS-wrapped DEK if SSH backend used
  uint32 workspace_size_mb = 9;
}

message SandboxHandle {
  string sandbox_id = 1;
  uint32 vsock_cid = 2;             // for Firecracker; 0 for GPU VMs
  string guest_ip = 3;               // for GPU VMs; empty for Firecracker
  uint32 fc_pid = 4;                 // 0 for GPU VMs
  google.protobuf.Timestamp ready_at = 5;
}

message SnapshotMetadata {
  string sandbox_id = 1;
  string local_path = 2;             // /var/lib/sc/snapshots/{id}/
  uint64 snapshot_size_bytes = 3;
  string snapshot_version = 4;       // Firecracker snapshot version, or "gpu-process-state"
  google.protobuf.Timestamp paused_at = 5;
}

message ResumeSpec {
  string sandbox_id = 1;
  string source = 2;                 // "host_local_ssd" or "gcs"
  string source_path = 3;            // local path or gs:// URL
}

message BalanceUpdate {
  string sandbox_id = 1;
  int64 balance_msats = 2;
  int64 consumed_msats_total = 3;
  google.protobuf.Timestamp metered_at = 4;
}

message HostStatus {
  string host_id = 1;
  uint64 total_mem_mb = 2;
  uint64 allocated_mem_mb = 3;
  uint32 firecracker_count = 4;
  string fc_version = 5;
  string ha_version = 6;
  bool draining = 7;
  repeated string sandbox_ids = 8;
}

message HostCommand {
  oneof cmd {
    DrainCommand drain = 1;
    AddSandboxOwnership add_ownership = 2;
    RemoveSandboxOwnership remove_ownership = 3;
  }
}
```

The Heartbeat is bidirectional: the host pushes status every 5s; the control plane piggybacks `add_ownership` / `remove_ownership` notifications when ring changes affect this host's sandboxes.

---

## Firecracker microVM Lifecycle (host-agent local)

The control plane never speaks to Firecracker directly. The orchestrator issues `host_agent.CreateSandbox(spec)` and the **host-agent** performs the steps below locally on its Firecracker Unix socket.

### Phase 1: Pre-Boot Configuration

Before issuing the `InstanceStart` action, the host-agent makes these Firecracker API calls in order:

**1. Machine Configuration**
- `PUT /machine-config`
- Fields: `vcpu_count`, `mem_size_mib`, `smt` (off for nano/micro)
- CPU oversubscription: nano tier uses `vcpu_count = 1` with cgroups CPU throttling on the host

**2. Boot Source**
- `PUT /boot-source`
- Fields: `kernel_image_path` (pre-built kernel on host), `boot_args`

**3. Root Drive**
- `PUT /drives/rootfs`
- `path_on_host` is the template root filesystem image; copy-on-write overlay per sandbox

**4. Workspace Drive**
- `PUT /drives/workspace`
- Blank ext4 image, 10GB (nano/micro) or 50GB (standard)
- Mounted at `/workspace` in the guest

**5. Network Interface**
- `PUT /network-interfaces/eth0`
- `host_dev_name`: pre-created TAP `tap-{sandbox_id}`
- iptables rules forbid cross-sandbox and guest→host traffic; egress disabled by default

**6. vsock Device**
- `PUT /vsock` — used by the host-agent for *all* exec/file/shell traffic to the guest

**7. Rate Limiter**
- `PUT /network-interfaces/eth0` (update with rate limiter)
- nano: 10 Mbps; micro: 50 Mbps; standard: 200 Mbps

### Phase 2: Boot

- `PUT /actions { "action_type": "InstanceStart" }`
- Cold start target: < 500ms via the snapshot-prewarming pool (10 pre-booted blank microVMs per host, reconfigured for each new request)

### Phase 3: Guest Agent

A small Go agent inside the guest:
- Listens on vsock port 1024 for `exec`, `write_file`, `read_file`, `metrics`, `pty` ops
- Manages `/workspace`
- Reports CPU/memory every 5s (the host-agent pulls these and stores them locally)
- Runs as `sandbox` (UID 1000), not root

### Phase 4: Running

The host-agent records:
- `host_vm_id`, `vsock_cid`, `fc_pid`
- Returns these to the control plane in `SandboxHandle`

The control plane writes them to `sandboxes` and adds the sandbox to its in-memory billing cache.

### Phase 5: Pause (Hot Snapshot to Local SSD)

On `host_agent.PauseSandbox(sandbox_id)`:
1. `PUT /actions { "action_type": "FlushMetrics" }`
2. `PUT /actions { "action_type": "Pause" }`
3. `PUT /snapshot/create` with:
   - `snapshot_path = /var/lib/sc/snapshots/{sandbox_id}/snap`
   - `mem_file_path = /var/lib/sc/snapshots/{sandbox_id}/mem`
   - `version = latest`
4. `PUT /actions { "action_type": "Resume" }` then `InstanceStop` — VM fully stopped
5. Release TAP device and vsock CID
6. Return `SnapshotMetadata { local_path, size_bytes }` to control plane immediately
7. Mark snapshot file with `pending_upload = true` xattr

Average pause API completion: < 1 second (no GCS upload on the critical path).

### Phase 6: Async GCS Upload (cold tier)

A goroutine on the host-agent scans `/var/lib/sc/snapshots/` every 60s. For any snapshot with `pending_upload = true` and `paused_at < now() - 5 min`:
1. Upload to `gs://sc-snapshots/{agent_id}/{sandbox_id}/{paused_at}/`
2. Once GCS upload returns 200, write a `gcs_path` xattr on the local file and clear `pending_upload`
3. Update the sandboxes row via the control plane: `gcs_snapshot_path = ...`, `gcs_uploaded_at = now()` (using `host_agent → control_plane → store` reverse-RPC, or a direct DB write with the read-only credentials gated to the `sandboxes.gcs_snapshot_path` column only)

If the host VM is decommissioned before the upload completes, the orchestrator marks the sandbox `snapshot_lost` and the agent must re-create.

### Phase 7: Restore (Hot or Cold Resume)

On `host_agent.ResumeSandbox({source: "host_local_ssd" | "gcs", source_path})`:

**Hot resume (same host, source = `host_local_ssd`):**
1. `PUT /snapshot/load { snapshot_path, mem_file_path, resume_vm = true }`
2. Reassign TAP and vsock CID
3. Return `SandboxHandle` in < 500ms

**Cold resume (different host, source = `gcs`):**
1. Stream snapshot files from GCS to local SSD (`gcsutil cp -m gs://... /var/lib/sc/snapshots/{id}/`)
2. Then proceed as hot resume
3. Total time dominated by GCS download (1–5s for typical micro snapshot)

The orchestrator decides hot vs. cold by checking `sandboxes.snapshot_node_host_id` against the live host pool; if the original host is `ready`, hot. Otherwise, pick a new host with capacity and request a cold resume.

### Phase 8: Termination

`host_agent.TerminateSandbox(sandbox_id)`:
1. `SendCtrlAltDel` with 2s timeout
2. If no response: force-kill Firecracker process
3. Remove TAP device
4. Delete CoW overlay and workspace disks
5. Delete local snapshot directory `/var/lib/sc/snapshots/{id}/`
6. The control plane separately: deletes any GCS snapshot, updates `sandboxes.status = 'terminated'`

---

## Exec / File / Shell Operations

These are **not** part of the orchestrator. They are HTTPS endpoints on the host-agent at `:8443`, reachable from the agent through `ilb-host-data` at `/h/{host_id}/sandbox/{id}/exec` etc.

The host-agent verifies the `data_jwt`, opens the per-sandbox vsock connection (cached), and forwards to the guest agent.

### Vsock Command Protocol (host-agent ↔ guest)

The host-agent opens a virtio-vsock connection to the guest using the sandbox's `vsock_cid` and port 1024.

Request (JSON line):
```
{ "op": "exec", "command": "pip install numpy", "timeout_ms": 30000, "env": {}, "dir": "/workspace" }
```

Response (streaming JSON lines):
```
{ "type": "stdout", "data": "Collecting numpy..." }
{ "type": "stderr", "data": "WARNING: ..." }
{ "type": "exit", "code": 0, "duration_ms": 4120 }
```

For exec calls reaching `/sandbox/{id}/exec`, the host-agent buffers stdout/stderr and returns one JSON response. For `/shell` (WSS), the JSON-line stream is converted to WebSocket frames in real time. See Component 8.

### Dedicated VM Exec (Replaces SSH)

Dedicated VMs (standard overflow, GPU tiers) are now the host-agent's *own* environment — there's no separate sandbox VM. The host-agent on a GPU VM:
- Spawns the user's exec command in a dedicated cgroup under user `sandbox` (UID 1000) with PID/network namespaces
- Streams output through a Unix pipe rather than vsock
- The HTTPS endpoint surface is identical to the Firecracker case

SSH server on dedicated VMs is **disabled by default**. It is provisioned only as an emergency operator backdoor (locked down to operator IPs + GCP IAP), and the SSH key encryption that follows applies only when the operator backdoor is used.

### File Operations (Firecracker — vsock)

Same JSON shapes as before:

```
{ "op": "write_file", "path": "/workspace/script.py", "content_b64": "...", "create_dirs": true }
```
→
```
{ "ok": true, "bytes_written": 1024 }
```

```
{ "op": "read_file", "path": "/workspace/output.csv" }
```
→
```
{ "ok": true, "content_b64": "...", "size_bytes": 4096 }
```

### Large File Streaming (> 50MB)

The host-agent's HTTP file endpoint accepts chunked request bodies. It splits into 4MB vsock chunks with CRC32 per chunk to the guest agent.

---

## Dedicated VM Lifecycle (GPU & Standard Overflow)

### Provisioning

Uses the GCP Compute Engine Go SDK (`cloud.google.com/go/compute/apiv1`).

**Instance configuration:**
- Machine type: `n1-standard-8` (gpu_small) or `a2-highgpu-1g` (gpu_large)
- Accelerator: `nvidia-tesla-t4` or `nvidia-a100-80gb`
- Boot disk: pre-baked custom image (`sockt-gpu-cuda12-v{git_sha}`):
  - Ubuntu 22.04 LTS
  - CUDA 12.2 + cuDNN 8.9 + NVIDIA driver 535+
  - PyTorch 2.x (for `ml-cuda` template)
  - host-agent binary at `/usr/local/bin/host-agent` with GPU-mode flag
- Network: `sc-gpu` subnet (10.3.0.0/20)
- Service account: `sa-gpu-host` (no GCP API access from inside the workload)
- Metadata: `host-id`, `control-plane-grpc=10.0.0.x:7444`, `sandbox-id`
- Startup script: enables nvidia persistence mode, starts `host-agent.service`

**Boot time target:** < 90 seconds (GPU driver init dominates).

### Communication

Dedicated VMs are reached over the same gRPC interface as Firecracker hosts. SSH is no longer used as a routine RPC channel.

### Termination

1. `compute.instances.delete`
2. Wait for op completion (async polling, timeout 300s)
3. Delete persistent disks (auto-delete is set at creation)
4. The host-agent deregisters from `host_vms` and is removed from `ilb-host-data` NEG by the reconciler

---

## Host VM Pool Management

### Pool Initialization

On control-plane startup:
1. Query Cloud SQL `host_vms` for all rows with `status = 'ready'`
2. Open persistent gRPC channels to each
3. For any host that doesn't accept the gRPC handshake within 5s, mark it `unreachable`

### Host Heartbeat (gRPC stream)

Each host-agent opens a long-lived bidirectional `Heartbeat` stream to a randomly selected control-plane instance on startup. Every 5 seconds it pushes a `HostStatus` message:

```
{
  host_id: "vm-host-pool-001",
  total_mem_mb: 64000,
  allocated_mem_mb: 24576,
  firecracker_count: 24,
  fc_version: "1.8.0",
  ha_version: "v2.1.0",
  draining: false,
  sandbox_ids: ["uuid", "uuid", ...]
}
```

The receiving control-plane instance updates the `host_vms` row in Cloud SQL and broadcasts the new state to peer instances via memberlist (so the in-memory `HostPool` is consistent across the MIG).

If the connection drops, the host-agent reconnects with exponential backoff to a different control-plane instance. After 30s without a successful heartbeat from any instance, the control plane marks the host `unreachable` and stops scheduling sandboxes there.

### Capacity Check Algorithm

When allocating a Firecracker microVM for a sandbox:

1. Filter `HostPool` to hosts with `status = 'ready'` and `kind = 'firecracker'`
2. Filter to hosts where `total_mem_mb - allocated_mem_mb >= sandbox.required_mem_mb`
3. Sort by `allocated_mem_mb DESC` (bin-packing)
4. Take the top host
5. Optimistically increment `allocated_mem_mb` in memory
6. Issue gRPC `CreateSandbox`; on failure decrement and try the next host

### Host VM Auto-Scaling

The orchestrator publishes a custom metric `host_pool_demand_pct` (allocated / total across all `ready` Firecracker hosts) every 30s. The `mig-fc-hosts` MIG autoscaler consumes this metric:

- **Scale-out**: `host_pool_demand_pct > 75%` for > 60s → MIG adds 2 instances
- **Scale-in**: `host_pool_demand_pct < 30%` for > 30 min → MIG removes 1 instance (most-empty drained first)

Drain procedure:
1. Orchestrator marks the chosen host `draining`
2. Orchestrator stops scheduling new sandboxes on it
3. Existing sandboxes are migrated (pause → upload to GCS → resume on a different host) over up to 5 min
4. After all sandboxes are gone, MIG deletes the instance

GPU instances continue to be created/destroyed individually by the orchestrator (no MIG).

---

## Sandbox Metrics

The host-agent collects per-sandbox metrics every 5 seconds via the guest agent (Firecracker) or local OS APIs (GPU VM):

- `cpu_percent`, `memory_used_mb`, `disk_used_mb`, `process_count`, `network_rx_bytes`, `network_tx_bytes`
- (GPU) `gpu_util_percent`, `gpu_mem_used_mb` via `nvidia-smi`

These are stored in-memory on the host-agent and returned in real time by `GET /sandbox/{id}/status`. They are **not** persisted to Cloud SQL (no time-series DB needed for v1). Aggregate metrics are exported as Prometheus gauges.

---

## Template Definitions

Templates are pre-baked root filesystem images stored in `gs://sc-templates/` and pre-fetched onto each host VM at first boot.

| Template ID | Base | Pre-installed Packages |
|---|---|---|
| `ubuntu-base` | Ubuntu 22.04 | bash, curl, wget, git, vim, python3-minimal |
| `python3` | ubuntu-base | python3.11, pip, virtualenv, numpy, pandas, requests |
| `nodejs` | ubuntu-base | Node.js 20 LTS, npm, yarn |
| `rust` | ubuntu-base | rustc 1.78, cargo, build-essential |
| `ml-cuda` | Ubuntu 22.04 + CUDA 12.2 | Python 3.11, PyTorch 2.x, HuggingFace transformers, accelerate, numpy |

Template images are versioned. The current version per template lives in Cloud SQL `templates` table.

### Templates Table Schema

| Column | Type | Description |
|---|---|---|
| `id` | TEXT | Template identifier (`python3`) |
| `display_name` | TEXT | Human-readable name |
| `description` | TEXT | What is pre-installed |
| `image_gcs_path` | TEXT | GCS path to the root FS image |
| `image_size_gb` | INT | Uncompressed image size |
| `compatible_tiers` | TEXT[] | Which tiers can use this template |
| `version` | TEXT | Semver version |
| `created_at` | TIMESTAMPTZ | Build time |

---

## Security Model

### Firecracker Isolation

- Each sandbox runs in its own microVM — separate kernel and memory space
- Jailer process is used in production: cgroups limits, dropped privileges, seccomp filter
- Host filesystem not accessible from the guest
- Inter-sandbox traffic blocked at the TAP bridge (iptables `FORWARD` rules)
- Egress disabled by default; enabled only with the egress add-on

### Dedicated VM Isolation

- Each GPU/standard-overflow sandbox is its own GCP VM
- The workload runs as `sandbox` (UID 1000) under cgroups + namespaces inside the host-agent
- The VM service account has no GCP API permissions
- VPC firewall: only allows ingress from `ilb-host-data` and from the control-plane CIDR; egress blocked unless egress is purchased

### SSH Key Management (Operator Backdoor Only)

SSH is no longer routine. When an operator needs SSH into a dedicated VM (incident response):
- Per-VM Ed25519 keypair generated at provision time
- Per-sandbox **DEK** generated from `crypto/rand`
- DEK wrapped via GCP KMS (`projects/.../keyRings/sc/cryptoKeys/sandbox-ssh-key`)
- Wrapped DEK stored in `sandboxes.ssh_private_key_wrapped` (BYTEA)
- Decryption requires `roles/cloudkms.cryptoKeyDecrypter` on the operator's identity (audited via Cloud Audit Logs)
- Plaintext key never persisted on disk; lives only in process memory during the SSH session

This eliminates the v1 weakness of deriving the AES key from the (effectively public) BOLT12 offer hash.

---

## Observability

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `orch_sandbox_create_duration_ms` | Histogram | tier, runtime |
| `orch_sandbox_terminate_duration_ms` | Histogram | tier, runtime |
| `orch_pause_local_duration_ms` | Histogram | tier |
| `orch_resume_hot_duration_ms` | Histogram | tier |
| `orch_resume_cold_duration_ms` | Histogram | tier |
| `orch_host_pool_utilization` | Gauge | zone |
| `orch_host_pool_demand_pct` | Gauge | — |
| `orch_firecracker_count` | Gauge | host_id |
| `orch_snapshot_local_bytes` | Histogram | tier |
| `orch_snapshot_gcs_upload_duration_ms` | Histogram | tier |
| `orch_host_unreachable_total` | Counter | host_id |
| `orch_host_grpc_errors_total` | Counter | host_id, op |

### Log Events

| Event | Level |
|---|---|
| `orch.sandbox.created` | INFO |
| `orch.sandbox.terminated` | INFO |
| `orch.snapshot.local_written` | INFO |
| `orch.snapshot.gcs_uploaded` | INFO |
| `orch.snapshot.lost` | ERROR |
| `orch.host.unreachable` | ERROR |
| `orch.host.scaled_out` | INFO |
| `orch.host.scaled_in` | INFO |
| `orch.gpu_vm.provisioning_failed` | ERROR |

---

## Internal Endpoints

The orchestrator exposes a small set of internal-only HTTP endpoints (VPC-firewalled, not on the global LB):

| Method | Path | Description |
|---|---|---|
| `GET` | `/internal/orchestrator/pool` | Current host pool state (for monitoring) |
| `POST` | `/internal/orchestrator/drain/{host_id}` | Mark host as draining |
| `GET` | `/internal/orchestrator/host_demand_pct` | Custom-metric scrape endpoint for the MIG autoscaler |

---

## Dependencies

- **Store Module** — for sandbox and host_vm state persistence (Cloud SQL, Component 6)
- **GCP Compute Engine API** — for dedicated VM provisioning and host pool reconciliation
- **GCP Cloud Storage** — cold snapshot tier (`gs://sc-snapshots/`)
- **GCP KMS** — wrapping per-sandbox SSH DEKs
- **Host Agent** (Component 2 / `cmd/host-agent/`) — the gRPC + HTTPS counterparty on each VM
- **Cluster Module** (Component 2 / `internal/cluster/`) — sandbox ownership across control-plane instances
- **Billing Engine** (Component 3) — orchestrator calls `billing.DeregisterSandbox` on pause/terminate; pushes `UpdateBalance` to host-agents
