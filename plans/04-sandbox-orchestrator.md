# Component 4: Sandbox Orchestrator

## Overview

The Sandbox Orchestrator is the `internal/orchestrator` module within the unified Go binary. It is responsible for the complete lifecycle of every compute environment: provisioning, monitoring, executing commands, managing files, snapshotting, restoring, and terminating.

The orchestrator manages two fundamentally different runtime backends — **Firecracker microVMs** (for nano, micro, and optionally standard tiers) and **dedicated GCP Compute Engine VMs** (for standard overflow and all GPU tiers) — through a unified interface that hides these differences from the rest of the system.

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

### Orchestrator

The top-level struct. Holds:
- `Store` interface reference — for reading/writing host_vms and sandbox state
- `FirecrackerPool` — manages connections to Firecracker host VMs
- `GCPComputeClient` — GCP Compute Engine API client (from `cloud.google.com/go/compute`)
- `GCSClient` — Google Cloud Storage client for snapshot uploads/downloads
- Config: `GCPProject`, `GCPZone`, `SnapshotBucket`, `VPCNetwork`, `VPCSubnet`

### FirecrackerPool

Maintains an in-memory map of host VMs and their current capacity. Synchronized with Supabase `host_vms` table on startup and via 30-second heartbeat updates.

Fields per host entry:
- `hostID` — GCP instance name
- `internalIP` — VPC IP for SSH tunnel to Firecracker sockets
- `totalMemMB`, `allocatedMemMB` — capacity tracking
- `firecrackerCount` — number of active microVMs
- `status` — `ready | draining | unreachable`
- `lastHeartbeat` — timestamp

### FirecrackerClient

One per active microVM. Communicates with the Firecracker REST API on the host VM via a Unix domain socket exposed over an SSH port forward or via a gRPC control agent running on each host.

---

## Firecracker microVM Lifecycle

### Phase 1: Pre-Boot Configuration

Before issuing the `InstanceStart` action, the Firecracker API must receive these configuration calls in order:

**1. Machine Configuration**
- `PUT /machine-config`
- Fields: `vcpu_count`, `mem_size_mib`, `smt` (simultaneous multi-threading, disabled for nano/micro)
- CPU oversubscription: nano tier uses `vcpu_count = 1` with Firecracker's CPU throttling via cgroups on the host

**2. Boot Source**
- `PUT /boot-source`
- Fields: `kernel_image_path` (pre-built kernel on host), `boot_args` (minimal init flags)
- Kernel is pre-compiled and cached on host — no download needed

**3. Root Drive**
- `PUT /drives/rootfs`
- Fields: `drive_id = "rootfs"`, `path_on_host` (template root filesystem image), `is_read_only = false`
- Root filesystems are template-specific (python3, nodejs, etc.) and pre-staged on each host VM
- Each sandbox gets a **copy-on-write overlay** so the template image is never modified

**4. Additional Drive (workspace)**
- `PUT /drives/workspace`
- A blank ext4 image, size: 10GB for nano/micro, 50GB for standard
- Mounted at `/workspace` inside the guest

**5. Network Interface**
- `PUT /network-interfaces/eth0`
- `host_dev_name`: a pre-created TAP device (`tap-{sandboxID}`) on the host
- Guest MAC address: deterministic from sandbox_id
- The host's iptables routes sandbox egress through a private bridge; internet access blocked by default

**6. vsock Device**
- `PUT /vsock` — enables virtio-vsock for host↔guest communication
- Used by the WebSocket shell proxy to tunnel stdin/stdout without TAP networking

**7. Rate Limiter**
- `PUT /network-interfaces/eth0` (update with rate limiter)
- Bandwidth limits per tier (nano: 10 Mbps, micro: 50 Mbps, standard: 200 Mbps)
- Packet-per-second limits to prevent DDoS from within sandboxes

### Phase 2: Boot

- `PUT /actions { "action_type": "InstanceStart" }`
- Cold start target: < 500ms from API call to guest SSH/vsock ready
- Achieved via Firecracker's snapshot pre-warming: on host startup, 10 "blank" microVMs are pre-booted and kept warm, ready to be reconfigured for the next sandbox request

### Phase 3: Guest Agent

After boot, a lightweight Go agent binary running inside the guest VM:
- Listens on the vsock for `exec`, `write_file`, `read_file`, `metrics` commands
- Manages `/workspace` directory
- Reports CPU and memory usage every 5 seconds to the host orchestrator
- Does not run as root; sandbox user is `sandbox` (UID 1000) in a separate network namespace

### Phase 4: Running

The orchestrator records:
- `host_vm_id` — which host this microVM is on
- `guest_ip` — TAP bridge IP assigned to guest
- `vsock_cid` — virtio-vsock context ID for shell proxy
- `wss_token` — bearer token for WebSocket shell connection

### Phase 5: Snapshot (Pause)

1. Issue `PUT /actions { "action_type": "FlushMetrics" }` — drain in-flight metrics
2. Issue `PUT /actions { "action_type": "Pause" }` — freeze VM execution
3. Issue `PUT /snapshot/create` with:
   - `snapshot_path` — path on host to write VM state file
   - `mem_file_path` — path to write memory dump file
   - `version` — Firecracker snapshot version
4. Upload both files to GCS: `gs://sc-snapshots/{agent_id}/{sandbox_id}/{timestamp}/`
5. Issue `PUT /actions { "action_type": "Resume" }` then immediately `InstanceStop` — the VM is fully stopped
6. Release TAP device, vsock CID, and host RAM allocation
7. Update `host_vms.allocated_mem_mb -= sandbox.mem_mb`

### Phase 6: Restore (Resume)

1. Download snapshot files from GCS to a temp directory on a host VM with sufficient capacity
2. Configure a new Firecracker process with `PUT /snapshot/load`:
   - `snapshot_path`, `mem_file_path`
   - `enable_diff_snapshots = false`
   - `resume_vm = true`
3. Reassign a TAP device and vsock CID
4. Update sandbox record: new `host_vm_id`, `guest_ip`, `wss_token`

### Phase 7: Termination

1. Issue `PUT /actions { "action_type": "SendCtrlAltDel" }` — graceful guest shutdown (2s timeout)
2. If no response: `DELETE /` — force-kill Firecracker process
3. Remove TAP device (`ip tuntap del name tap-{id}`)
4. Delete copy-on-write overlay disk image
5. Delete workspace disk image
6. Delete GCS snapshot if present
7. Update `host_vms.allocated_mem_mb -= sandbox.mem_mb`
8. Update sandbox `status = 'terminated'`, `terminated_at`

---

## Exec Command

The orchestrator executes commands inside a sandbox via the guest agent over vsock.

### vsock Command Protocol

The host orchestrator opens a virtio-vsock connection to the guest agent using the sandbox's `vsock_cid` and port 1024.

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

### Dedicated VM Exec

For dedicated VMs (standard overflow, GPU tiers), the orchestrator executes commands via SSH using a per-sandbox keypair:
- SSH target: `sandbox@{guest_ip}:22`
- Key: ephemeral Ed25519 keypair generated at sandbox creation; private key stored encrypted in Supabase `sandboxes.ssh_private_key_enc`
- Command wrapped in: `timeout {timeout_ms}ms bash -c '{command}'`

---

## File Operations

### Write File (Firecracker — vsock)

Request:
```
{ "op": "write_file", "path": "/workspace/script.py", "content_b64": "...", "create_dirs": true }
```

Response:
```
{ "ok": true, "bytes_written": 1024 }
```

### Read File (Firecracker — vsock)

Request:
```
{ "op": "read_file", "path": "/workspace/output.csv" }
```

Response:
```
{ "ok": true, "content_b64": "...", "size_bytes": 4096 }
```

### Large File Streaming (> 50MB)

Files over 50MB use a chunked transfer protocol over vsock, splitting into 4MB chunks with a CRC32 checksum per chunk. The HTTP layer accepts chunked uploads and streams them to vsock.

---

## Dedicated VM Lifecycle (GPU & Standard Overflow)

### Provisioning

Uses the GCP Compute Engine Go SDK (`cloud.google.com/go/compute/apiv1`).

**Instance configuration:**
- Machine type: `n1-standard-8` (gpu_small) or `a2-highgpu-1g` (gpu_large)
- Accelerator: `nvidia-tesla-t4` (1 unit) or `nvidia-a100-80gb` (1 unit)
- Boot disk: pre-baked custom image with:
  - Ubuntu 22.04 LTS
  - CUDA 12.2 + cuDNN 8.9
  - PyTorch 2.x (for ml-cuda template)
  - NVIDIA drivers 535+
  - Guest agent binary (Go, same as Firecracker guest agent)
  - SSH server configured with ephemeral key injection via metadata
- Network interface: attached to `sc-gpu` VPC subnet (10.3.0.0/20)
- Service account: minimal IAM — no GCP API access from inside the sandbox
- Metadata: `ssh-keys = sandbox:{agent_pubkey}`, `sandbox-id = {uuid}`
- Startup script: installs any template-specific packages, starts guest agent

**Boot time target:** < 90 seconds (GPU driver initialization is the bottleneck)

### Communication

Dedicated VMs expose the guest agent over both vsock (where supported) and SSH. The orchestrator prefers SSH for dedicated VMs since they are full VMs with a complete SSH stack.

### Termination

1. Call GCP `compute.instances.delete` API
2. Wait for operation to complete (async polling, timeout 300s)
3. Delete any associated persistent disks that were not auto-deleted
4. Update `sandboxes` and release from `host_vms` pool (dedicated VMs are not pooled — each is created/destroyed per session)

---

## Host VM Pool Management

### Pool Initialization

On binary startup, the orchestrator:
1. Queries Supabase `host_vms` for all rows with `status = 'ready'`
2. Loads all rows into the in-memory `FirecrackerPool` map
3. Queries GCP Compute Engine API to verify each host is actually running
4. Marks any hosts not found in GCP as `status = 'unreachable'` in Supabase

### Host Heartbeat

Every 30 seconds, each host VM runs a systemd service that POSTs to an internal heartbeat endpoint on the Go binary:

`POST /internal/host/heartbeat`

Body:
```
{
  "host_id": "vm-host-pool-001",
  "total_mem_mb": 64000,
  "allocated_mem_mb": 24576,
  "firecracker_count": 24,
  "kernel_version": "6.1.94",
  "firecracker_version": "1.8.0"
}
```

The orchestrator updates `host_vms` in Supabase and refreshes the in-memory pool entry.

### Capacity Check Algorithm

When allocating a Firecracker microVM for a sandbox:

1. Filter `FirecrackerPool` to hosts with `status = 'ready'`
2. Filter to hosts where `total_mem_mb - allocated_mem_mb >= sandbox.required_mem_mb`
3. Sort by `allocated_mem_mb DESC` (bin-packing: prefer the most-loaded host to leave empty hosts free for scale-in)
4. Select the top host
5. Optimistically increment `allocated_mem_mb` in memory before Firecracker creation
6. If Firecracker creation fails: decrement and try next host

### Host VM Auto-Scaling

The orchestrator monitors pool utilization and calls the GCP Compute Engine API to provision new host VMs when needed.

**Scale-out trigger:** Pool utilization > 75% (allocated / total across all hosts)
- Provision 2 new `c2-standard-16` VMs from the nested-KVM image
- Wait for heartbeat registration (< 120s)

**Scale-in trigger:** Any host VM with < 10% utilization for > 30 minutes AND pool has > 2 hosts
- Mark host as `status = 'draining'`
- Wait for all Firecracker VMs on that host to be terminated or migrated
- Delete the GCP instance

---

## Sandbox Metrics

The orchestrator collects VM metrics from each active sandbox via the guest agent every 5 seconds:

**Firecracker guest agent reports:**
- `cpu_percent` — CPU usage over last 5s
- `memory_used_mb` — guest RSS
- `disk_used_mb` — workspace disk usage
- `process_count` — number of processes in guest
- `network_rx_bytes`, `network_tx_bytes` — since last report

**Dedicated VM metrics** are collected via SSH: `cat /proc/loadavg`, `free -m`, `df /workspace`, `nvidia-smi` (GPU tier).

The metrics are:
1. Stored in-memory in the orchestrator (most recent snapshot per sandbox)
2. Returned in real-time by `GET /v1/sandbox/{id}/status`
3. **Not** persisted to Supabase (no time-series DB needed for v1)

---

## Template Definitions

Templates are pre-baked root filesystem images staged on each host VM. They are stored in GCS and downloaded to hosts during host provisioning.

| Template ID | Base | Pre-installed Packages |
|---|---|---|
| `ubuntu-base` | Ubuntu 22.04 | bash, curl, wget, git, vim, python3-minimal |
| `python3` | ubuntu-base | python3.11, pip, virtualenv, numpy, pandas, requests |
| `nodejs` | ubuntu-base | Node.js 20 LTS, npm, yarn |
| `rust` | ubuntu-base | rustc 1.78, cargo, build-essential |
| `ml-cuda` | Ubuntu 22.04 + CUDA 12.2 | Python 3.11, PyTorch 2.x, HuggingFace transformers, accelerate, numpy |

Template images are versioned. The current version for each template is stored in Supabase `templates` table.

### Templates Table Schema

| Column | Type | Description |
|---|---|---|
| `id` | TEXT | Template identifier (e.g., `python3`) |
| `display_name` | TEXT | Human-readable name |
| `description` | TEXT | What is pre-installed |
| `image_gcs_path` | TEXT | GCS path to the root FS image |
| `image_size_gb` | INT | Uncompressed image size |
| `compatible_tiers` | TEXT[] | Which tiers can use this template |
| `version` | TEXT | Semver version of this image |
| `created_at` | TIMESTAMPTZ | When this version was built |

---

## Security Model

### Firecracker Isolation

- Each sandbox runs in its own microVM — separate kernel, separate memory space
- The Jailer process is used in production: sets `cgroups` limits, drops privileges, applies seccomp filter before launching Firecracker
- Host filesystem is not accessible from inside the guest
- Inter-sandbox networking is blocked at the TAP bridge level (iptables `FORWARD` rules)
- Egress to internet blocked by default; enabled only with the egress add-on (separate L402 payment)

### Dedicated VM Isolation

- Each GPU sandbox gets its own GCP VM instance — hardware isolation
- The sandbox user inside the VM has no sudo access
- The VM's service account has no IAM permissions (cannot call GCP APIs)
- VPC firewall rules allow only: SSH from orchestrator VPC IP, outbound to internet if egress enabled

### SSH Key Management

- Ephemeral Ed25519 keypair generated per sandbox at creation time
- Public key injected via GCP instance metadata
- Private key encrypted with AES-256 using a per-agent key derived from the agent's `bolt12_offer` hash
- Stored in `sandboxes.ssh_private_key_enc` in Supabase
- Decrypted only when needed for exec/file operations
- Deleted from Supabase on sandbox termination

---

## Observability

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `orchestrator_sandbox_create_duration_ms` | Histogram | tier, runtime |
| `orchestrator_sandbox_terminate_duration_ms` | Histogram | tier, runtime |
| `orchestrator_exec_duration_ms` | Histogram | tier |
| `orchestrator_host_pool_utilization` | Gauge | zone |
| `orchestrator_firecracker_count` | Gauge | host_id |
| `orchestrator_snapshot_upload_mb` | Histogram | tier |
| `orchestrator_snapshot_restore_duration_ms` | Histogram | tier |
| `orchestrator_host_unreachable_total` | Counter | host_id |

### Log Events

| Event | Level |
|---|---|
| `orchestrator.sandbox.created` | INFO |
| `orchestrator.sandbox.terminated` | INFO |
| `orchestrator.snapshot.created` | INFO |
| `orchestrator.snapshot.restored` | INFO |
| `orchestrator.host.unreachable` | ERROR |
| `orchestrator.host.scaled_out` | INFO |
| `orchestrator.host.scaled_in` | INFO |
| `orchestrator.exec.timeout` | WARN |
| `orchestrator.gpu_vm.provisioning_failed` | ERROR |

---

## Internal Endpoints

The orchestrator exposes internal-only HTTP endpoints (not gated by Aperture, reachable only from within the VPC):

| Method | Path | Description |
|---|---|---|
| `POST` | `/internal/host/heartbeat` | Host VM capacity update |
| `GET` | `/internal/orchestrator/pool` | Current host pool state (for monitoring) |
| `GET` | `/internal/orchestrator/tickers` | Active billing tickers (for monitoring) |
| `POST` | `/internal/orchestrator/drain/{host_id}` | Mark host as draining (scale-in prep) |

---

## Dependencies

- **Store Module** — for sandbox and host_vm state persistence
- **GCP Compute Engine API** — for dedicated VM provisioning and host pool scaling
- **GCP Cloud Storage** — for snapshot upload/download
- **Firecracker** (on host VMs) — via Unix socket API or SSH-tunneled HTTP
- **Guest Agent** (running inside each sandbox) — for exec, file, and metrics operations
- **Billing Engine** (Component 3) — orchestrator calls `billing.StopBilling` on pause/terminate
