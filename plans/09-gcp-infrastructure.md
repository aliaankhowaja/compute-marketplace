# Component 9: GCP Infrastructure

## v2 changelog

- Cloud Run replaced by a regional GCE Managed Instance Group (`mig-control-plane`, `n2-standard-4`, min 3 / max 30) for the control plane. Eliminates VPC-connector hop, CPU throttling, and the lack of instance affinity.
- New per-host data plane: every Firecracker host VM and every dedicated GPU VM runs the `host-agent` binary. A new internal HTTPS LB (`ilb-host-data`) routes `/h/{host_id}/*` traffic by path to the named host VM via a header-based backend selector.
- Cloud SQL for PostgreSQL added (regional, private IP, in `sc-control` subnet) for all hot-path tables. Supabase remains only as an external dependency for Auth.
- Aperture sidecar removed everywhere; nothing else changes about the global LB except enabling HTTP/3.
- LND deployed as an active-passive pair (`lnd-node-primary`, `lnd-node-secondary`) on dedicated VMs with shared `channel.backup`.
- New GCP KMS keyring `sc` and key `sandbox-ssh-key` for wrapping per-sandbox SSH DEKs.

---

## Overview

This component describes the complete Google Cloud Platform infrastructure for Sockt. It covers all GCP services, VPC network design, IAM configuration, scaling policies, and operational runbooks. The infrastructure is defined as code using Terraform and applied through Cloud Build.

The deployment splits into three planes:

- **Control plane** (`mig-control-plane`): a regional MIG running the unified Go control-plane binary. Receives external traffic via the global HTTPS LB; talks to LND, Cloud SQL, and host-agents over the in-VPC private network.
- **Host data plane** (`mig-fc-hosts` + dedicated GPU instances): every host VM runs `host-agent`, exposing both an mTLS gRPC server (for the control plane) and an HTTPS server (for client-direct exec/file/shell). Reachable from the global LB through the internal `ilb-host-data` for the data path, and from the control plane via private IP for the control path.
- **Lightning plane** (`lnd-node-primary` + `lnd-node-secondary`): the LND HA pair plus bitcoind plus LNDK on dedicated VMs.

---

## GCP Services Used

| Service | Role |
|---|---|
| Compute Engine MIG | Control plane (`mig-control-plane`); Firecracker host pool (`mig-fc-hosts`) |
| Compute Engine (single-instance) | LND HA pair, dedicated GPU VMs (created on demand) |
| Cloud SQL for PostgreSQL | All hot-path tables (agents, users, sandboxes, billing_intervals, credit_transactions, top_up_intervals, audit_log, ...) |
| Cloud Load Balancing | External global HTTPS LB (HTTP/3 enabled) for both control and host-data paths; internal regional HTTPS LB for host-data backends |
| VPC + Cloud NAT | Private networking; sandbox egress NAT |
| Cloud Storage | Cold sandbox snapshots, Firecracker template images, Terraform state, LND `channel.backup` |
| Cloud KMS | Per-sandbox SSH DEK wrapping; pgcrypto keys |
| Secret Manager | LND credentials, DB passwords, Supabase JWT secret, KMS configs |
| Artifact Registry | Docker images for control-plane, host-agent, sandbox templates |
| Cloud Build | CI/CD pipeline: build, test, migrate, deploy |
| Cloud Scheduler | Nightly reconciliation, snapshot cleanup, channel rebalancing |
| Cloud Armor | DDoS protection, IP-based rate limiting |
| Cloud Monitoring | Metrics, dashboards, alerting |
| Cloud Logging | Structured log aggregation |
| IAM | Service accounts with least-privilege roles |

(Cloud Run and Serverless VPC Connector are no longer used.)

---

## VPC Network Design

### Network: `sc-network`

A single VPC network with 5 subnets across 2 zones for availability.

### Subnets

| Subnet Name | CIDR | Region/Zone | Purpose |
|---|---|---|---|
| `sc-control` | `10.0.0.0/24` | us-central1-a, us-central1-b | Control-plane MIG, LND HA pair, Cloud SQL private endpoint |
| `sc-vm-hosts` | `10.1.0.0/20` | us-central1-a, us-central1-b | Firecracker host pool VMs |
| `sc-microvm` | `10.2.0.0/16` | N/A (virtual) | TAP interface IP space for Firecracker guests |
| `sc-gpu` | `10.3.0.0/20` | us-central1-a | Dedicated GPU VM instances |
| `sc-cloudsql` | `10.4.0.0/24` | us-central1 | Reserved for Cloud SQL private services access (PSA) |

### Cloud NAT Gateway

Two NAT gateways (one per zone for redundancy) provide outbound internet for:
- Firecracker host VMs to pull updates and template images from GCS
- Control plane VMs to reach Supabase Auth and external services
- GPU VMs when egress is enabled for a sandbox

Sandboxes do NOT get outbound internet by default. NAT is configured with subnet exclusions and iptables on the host VM control the per-sandbox egress.

### Private Services Access for Cloud SQL

Cloud SQL's private IP is reached via VPC peering (Private Services Access). The control plane MIG dials `10.4.0.3:5432` (private IP of the Cloud SQL primary) directly — no public IP, no Supavisor pooler, no cross-network hops.

### Private Google Access

Enabled on all subnets. Allows VMs without external IPs to reach Google APIs (Cloud Storage, Secret Manager, Artifact Registry, KMS) without traversing the public internet.

(VPC Serverless Connector removed — no longer needed without Cloud Run.)

---

## Firewall Rules

| Rule Name | Direction | Source | Destination | Ports | Purpose |
|---|---|---|---|---|---|
| `allow-cp-to-host-grpc` | INGRESS | `sc-control` CIDR | `vm-host` tag | TCP 7443 | Control plane → host-agent mTLS gRPC |
| `allow-clb-to-host-https` | INGRESS | GCP LB IP ranges | `vm-host` tag | TCP 8443 | Internal LB → host-agent HTTPS data plane |
| `allow-cp-to-lnd-grpc` | INGRESS | `sc-control` CIDR | `lnd-node` tag | TCP 10009 | Control plane → LND gRPC |
| `allow-cp-to-cloudsql` | INGRESS | `sc-control` CIDR | Cloud SQL private IP | TCP 5432 | Control plane → Cloud SQL |
| `allow-lnd-p2p` | INGRESS | All | `lnd-node` tag | TCP 9735 | Lightning Network P2P |
| `allow-host-to-gcs` | EGRESS | `vm-host` tag | GCS API IPs | TCP 443 | Image and snapshot access |
| `deny-microvm-to-hosts` | INGRESS | `sc-microvm` CIDR | `vm-host` tag | All | Prevent guest-to-host breakout |
| `deny-microvm-to-microvm` | INGRESS | `sc-microvm` CIDR | `sc-microvm` CIDR | All | Prevent cross-sandbox traffic |
| `allow-gpu-egress` | EGRESS | `sc-gpu` tag + `egress-enabled` metadata | All | TCP 80, 443 | Internet access for GPU sandboxes with egress purchased |
| `allow-cp-internal-grpc` | INGRESS | `sc-control` CIDR | `sc-control` CIDR | TCP 7444 | Control plane peer-to-peer (cluster membership, request forwarding) |
| `deny-all-ingress` | INGRESS | All | All | All | Default deny (lowest priority) |

---

## Compute Engine: Control Plane MIG

### Instance Specification (`mig-control-plane`)

| Property | Value |
|---|---|
| Machine type | `n2-standard-4` (4 vCPU, 16 GB RAM) |
| Image | Custom: `sockt-control-plane-v{git_sha}` (Container-Optimized OS or Ubuntu 22.04 minimal + control-plane binary) |
| Boot disk | 50 GB SSD (pd-balanced) |
| Network | `sc-control` subnet, no external IP |
| Service account | `sa-control-plane@{project}.iam.gserviceaccount.com` |
| Tags | `control-plane` |
| Min instances | 3 (regional, one per zone) |
| Max instances | 30 |
| Autoscaling signal | CPU utilization > 60% for > 60s OR active sandbox count per instance > 200 (custom metric) |
| Health check | TCP `:8081` + HTTP `GET /healthz` returns 200 |
| Update policy | Surge update; max-surge=3, max-unavailable=0; signal SIGTERM with 60s drain |

### Cluster Membership

The control-plane instances form a peer cluster using either Hashicorp memberlist (gossip) on port 7444 or a Cloud SQL `instance_leases` table heartbeated every 5s. Each instance publishes:

- `instance_id` (the GCE instance name)
- `private_ip` (for peer gRPC forwarding on port 7444)
- `started_at`, `last_heartbeat`

Sandbox ownership is computed locally as `instances[hash(sandbox_id) % len(instances)]` from the live membership list. On membership change (instance up/down), ownership rebalances; the new owner reads the sandbox's current state from Cloud SQL and resumes its in-memory ticker.

### Internal LB for Control Plane

| Property | Value |
|---|---|
| Type | Regional internal HTTPS LB |
| Name | `ilb-control-plane` |
| Backend | `mig-control-plane` |
| Health check | `GET /healthz` |
| Used by | Global external LB as a backend for `/v1/*` and `/mcp/*` paths |

---

## Compute Engine: Firecracker Host Pool MIG

### Instance Specification (`mig-fc-hosts`)

| Property | Value |
|---|---|
| Machine type | `c2-standard-16` (16 vCPU, 64 GB RAM) |
| Image | Custom: `sockt-fc-host-v{git_sha}` (Ubuntu 22.04 + nested KVM license + Firecracker + host-agent) |
| Boot disk | 100 GB SSD (pd-ssd) |
| Local SSD | 1× 375 GB NVMe local SSD for hot snapshot storage at `/var/lib/sc/snapshots/` |
| Data disk | 500 GB SSD (pd-ssd) — for Firecracker template images and overlay disks |
| Network | `sc-vm-hosts` subnet, no external IP |
| Service account | `sa-vm-host@{project}.iam.gserviceaccount.com` |
| Tags | `vm-host`, `fc-host` |
| Min instances | 4 |
| Max instances | 200 |
| Autoscaling | Driven by control-plane orchestrator via direct MIG resize calls (richer than CPU autoscaler — see Component 4) |

### Custom Image Build: `sockt-fc-host-v{git_sha}`

Built by Cloud Build and stored in Artifact Registry. Building involves:

1. Start from `ubuntu-2204-lts` base image
2. Enable nested virtualization via GCP license flag
3. Install: Firecracker binary (latest stable), KVM kernel modules, `iproute2` (TAP), `e2fsprogs` (disk images)
4. Pre-download Firecracker guest kernel image and template root filesystems from `gs://sc-templates/`
5. Install the `host-agent` binary at `/usr/local/bin/host-agent` (from Artifact Registry image extraction)
6. Configure systemd services:
   - `host-agent.service` — main host-agent process (gRPC :7443 + HTTPS :8443)
   - `sc-iptables.service` — load sandbox network isolation rules at boot
7. Mount local NVMe SSD at `/var/lib/sc/snapshots/` (formatted ext4 at first boot via cloud-init)

### Internal LB for Host Data Plane

| Property | Value |
|---|---|
| Type | Regional internal HTTPS LB |
| Name | `ilb-host-data` |
| Backend selection | URL map: `/h/{host_id}/*` → backend NEG containing the named host instance |
| Health check | `GET /healthz` on `:8443` |
| TLS | Per-host certificate signed by internal CA (rotated weekly) |
| Used by | Global external LB as a backend for `/h/*` paths |

The URL map uses path-template rewriting: `/h/{host_id}/sandbox/{sandbox_id}/exec` is forwarded to the matching host's `:8443` as `/sandbox/{sandbox_id}/exec`. Backend NEGs are auto-populated by a control-plane reconciler that watches `host_vms` table changes and updates NEG membership via the GCP Compute API.

### Host VM Capacity

Each `c2-standard-16` host supports:
- Nano sandboxes (256 MB each): up to ~240
- Micro sandboxes (1 GB each): up to ~59
- Standard sandboxes (4 GB each): up to ~14
- Mixed workloads: bin-packing determines actual capacity

---

## Compute Engine: LND Lightning Node (HA Pair)

| Property | `lnd-node-primary` | `lnd-node-secondary` |
|---|---|---|
| Machine type | `n2-standard-4` (4 vCPU, 16 GB RAM) | `n2-standard-4` |
| Image | Ubuntu 22.04 LTS | Ubuntu 22.04 LTS |
| Boot disk | 500 GB SSD | 500 GB SSD |
| Internal IP | `10.0.0.5` (static) | `10.0.0.6` (static) |
| External IP | Static external (LN P2P) | Static external (LN P2P) |
| Tags | `lnd-node`, `lnd-active` (label managed by failover script) | `lnd-node` |
| Service account | `sa-lnd@{project}.iam.gserviceaccount.com` | `sa-lnd@{project}.iam.gserviceaccount.com` |

### LND Setup

Each LND VM runs three processes managed by systemd:
1. `bitcoind` — pruned Bitcoin Core node
2. `lnd` — Lightning Network Daemon
3. `lndk` — BOLT12 onion messaging sidecar

Both nodes operate from the **same node identity** (shared `tls.cert`, `tls.key`, `wallet.db`, and `channel.backup` synced via GCS). At any time, exactly one is the active node and serves gRPC; the other has its `lnd` process stopped. A failover daemon (`sc-lnd-failover.service`) on each node monitors the peer; on primary failure it promotes the secondary by:

1. Stopping `lnd` on the dead primary (or proceeding if unreachable)
2. Restoring the latest `channel.backup` from GCS on the secondary
3. Starting `lnd` on the secondary
4. Updating the `lnd-active` GCE label
5. Notifying the control plane via PubSub so it reconnects gRPC to the new active node's IP

The control plane's payment module discovers the active node via the `lnd-active` GCE label (queried at startup and refreshed on gRPC connection error).

### LND Backup Strategy

- `channel.backup` is uploaded to `gs://sc-lnd-backups/` every 5 minutes by a cron job on the active node
- On any LND restart, the node checks for a newer backup in GCS before starting

---

## Compute Engine: GPU Pool

GPU instances are NOT pre-provisioned. They are created on demand by the orchestrator when a `gpu_small` or `gpu_large` sandbox is requested. Every GPU VM runs the same `host-agent` binary as the Firecracker host pool, registers itself with the control plane on boot, and is added to the `ilb-host-data` backend NEG by the reconciler.

### GPU Instance Specifications

| Tier | Machine Type | GPU | GPU Count | RAM |
|---|---|---|---|---|
| `gpu_small` | `n1-standard-8` | NVIDIA Tesla T4 | 1 | 30 GB |
| `gpu_large` | `a2-highgpu-1g` | NVIDIA A100 80GB | 1 | 85 GB |

### GPU Custom Image: `sockt-gpu-cuda12-v{git_sha}`

Pre-baked image stored in Artifact Registry. Contains:
- Ubuntu 22.04 LTS
- CUDA 12.2 Toolkit, cuDNN 8.9, NVIDIA Driver 535+
- Python 3.11, pip, PyTorch 2.x, HuggingFace Transformers, Accelerate, Datasets
- `host-agent` binary at `/usr/local/bin/host-agent` (same as Firecracker hosts, with GPU-mode flag)
- Local sandbox runner (the host-agent on a GPU VM treats the VM itself as the sandbox boundary; no Firecracker)

Image build time: ~45 minutes. Rebuild triggered by Dockerfile changes in Cloud Build.

### GPU Instance Lifecycle

1. Orchestrator calls `compute.instances.insert` with the GPU spec and metadata `{"sandbox_id": "...", "control_plane_grpc": "10.0.0.x:7444"}`
2. GCP provisions the VM (30–90 seconds)
3. Startup script: `nvidia-smi -pm 1`, start `host-agent.service`
4. host-agent registers with control plane via gRPC `RegisterHost(host_id, capabilities=[gpu_small])` and is added to `ilb-host-data` NEG
5. Orchestrator marks sandbox `running` and returns `data_endpoint`
6. On termination: `compute.instances.delete` — disks auto-deleted

---

## Cloud SQL for PostgreSQL

### Instance Specification

| Property | Value |
|---|---|
| Instance ID | `sc-cloudsql-primary` |
| Database version | PostgreSQL 16 |
| Region | us-central1 (regional HA: failover replica in us-central1-b) |
| Machine type | `db-custom-8-32768` (8 vCPU, 32 GB RAM) — scale up as needed |
| Storage | 500 GB SSD, automatic resize enabled |
| Networking | Private IP only (`10.4.0.3`), via Private Services Access; no public IP |
| Backups | Daily automated, 14-day retention; PITR enabled |
| Encryption | Google-managed (CMEK optional) |

### Connection Strategy

The control-plane Go binary uses `pgx` with a per-instance pool:
- `MaxConns = 25`, `MinConns = 5`
- Statement cache enabled
- TLS required (Cloud SQL native TLS via the in-VPC private IP — no Cloud SQL Auth Proxy needed)

`pg_advisory_xact_lock` works directly against this pool (no transaction-mode pooler in between).

### Read Replicas

A read replica (`sc-cloudsql-replica-1`) in us-central1-b serves heavy dashboard read queries and the nightly reconciliation job. The control plane targets the primary for all writes and most reads, falling back to the replica only for `/v1/billing/history` and `/v1/user/credits/history` (paginated reads).

---

## Cloud Run Service

Removed. The control plane runs on `mig-control-plane` instead. Cloud Run no longer appears in the deployment.

---

## Cloud Storage Buckets

### `sc-snapshots`

| Property | Value |
|---|---|
| Location | US-CENTRAL1 (regional) |
| Storage class | STANDARD |
| Versioning | Disabled (single snapshot per sandbox) |
| Lifecycle rules | Delete objects > 48h old |
| Access | Private; accessed by `sa-vm-host` (write) and `sa-control-plane` (read) |
| Encryption | Google-managed (CMEK optional for enterprise tier) |

Snapshots are written to host-local SSD first (see Component 4); GCS is the cold tier only.

### `sc-templates`

| Property | Value |
|---|---|
| Location | US-CENTRAL1 |
| Storage class | STANDARD |
| Content | Firecracker kernel images and root FS images |
| Access | Read by `sa-vm-host`; write by Cloud Build only |

### `sc-scripts`

| Property | Value |
|---|---|
| Content | Host VM startup scripts, iptables rules |
| Access | Read by `sa-vm-host`; write by Cloud Build |

### `sc-lnd-backups`

| Property | Value |
|---|---|
| Content | LND `channel.backup` snapshots |
| Versioning | Enabled (recovery) |
| Lifecycle rules | Keep last 100 versions |
| Access | Read/write by `sa-lnd` only |

### `sc-tf-state`

| Property | Value |
|---|---|
| Content | Terraform state files |
| Versioning | Enabled |
| Access | Read/write by Terraform service account only |

---

## Cloud Load Balancing

### Frontend (External Global HTTPS LB)

- Anycast global IP
- HTTP/2 and **HTTP/3 (QUIC)** enabled
- SSL policy: `MODERN` (TLS 1.2+, strong cipher suites)
- Managed SSL certificate for `api.sockt.dev`
- WebSocket and SSE explicitly enabled (`--connection-draining-timeout=300s`)

### URL Map

| Path Match | Backend |
|---|---|
| `/v1/*`, `/mcp/*`, `/healthz`, `/internal/webhooks/stripe` | `ilb-control-plane` (regional internal HTTPS) |
| `/h/{host_id}/*` | `ilb-host-data` (regional internal HTTPS) — header injection: `X-Sc-Host: {host_id}` |
| Default | 404 |

### Backend Service Configuration

- Timeout: 600 seconds (long exec calls and WSS sessions)
- Health check: `GET /healthz` on each backend ILB
- Connection draining: 300s on instance removal

### Cloud Armor Policy

Applied to the frontend load balancer:

| Rule | Priority | Action | Description |
|---|---|---|---|
| Rate limit by IP | 1000 | Throttle (429) | > 300 requests/minute per IP |
| Block Tor exit nodes | 2000 | Deny (403) | Optional; configurable |
| Allow all | 9999 | Allow | Default allow |

---

## Cloud KMS

| KeyRing | Key | Purpose |
|---|---|---|
| `sc` | `sandbox-ssh-key` | Wraps per-sandbox SSH DEKs (Component 4) |
| `sc` | `pgcrypto-data-key` | Wraps any column-level encrypted DB columns |
| `sc` | `wss-jwt-signing-key` | (Optional) HMAC-key wrapped at rest; per-instance signing keys derived from this |

KMS is reached via Private Google Access from `sc-control` and `sc-vm-hosts`.

---

## Secret Manager

All secrets are stored as Secret Manager secrets with automatic rotation reminders.

| Secret Name | Content | Rotation |
|---|---|---|
| `cloudsql-password` | Cloud SQL master password | 90 days |
| `supabase-jwt-secret` | Supabase Auth JWT signing secret (verify-only on control plane) | 90 days |
| `lnd-tls-cert` | LND TLS certificate PEM (shared by HA pair) | On LND cert renewal |
| `lnd-invoice-macaroon` | Scoped LND macaroon (shared by HA pair) | On LND restart |
| `wss-jwt-secret` | HMAC secret for `wss_token` and host-data-plane JWT signing | 90 days |
| `host-agent-mtls-ca` | Internal CA cert + key (signs host-agent server certs) | 365 days |
| `stripe-webhook-secret` | Stripe webhook signing secret | 180 days |

---

## Cloud Build CI/CD Pipeline

### Trigger

Any push to `main` triggers the pipeline.

### Build Steps

1. **Test** — Run Go unit and integration tests against an ephemeral Cloud SQL test instance
2. **Build** — `docker build` for control plane and host-agent (separate images)
3. **Push** — Push images to Artifact Registry with git SHA tag
4. **Migrate** — Run `dbmate up` against Cloud SQL primary (with dry-run first)
5. **Deploy control plane** — Update `mig-control-plane` instance template with new control-plane image; rolling surge update (max-surge=3, max-unavailable=0)
6. **Deploy host-agent** — Build new `sockt-fc-host-v{git_sha}` image; rolling surge update of `mig-fc-hosts`; new GPU image baked for next GPU instance create
7. **Smoke test** — Hit `/healthz` on every new control-plane instance; verify LND and DB connectivity
8. **Rollback** — On failure, MIG rollback to previous instance template

### Cloud Build Service Account

Has `roles/compute.instanceAdmin.v1`, `roles/compute.instanceTemplateAdmin`, `roles/storage.admin` (state), `roles/cloudsql.admin`, `roles/artifactregistry.writer`.

---

## Cloud Scheduler Jobs

| Job Name | Schedule | Target | Purpose |
|---|---|---|---|
| `billing-reconciliation` | `0 2 * * *` (2am UTC) | Cloud Run Job | Find and retry unsettled top-ups; reconcile in-memory balances vs DB |
| `snapshot-cleanup` | `0 3 * * *` (3am UTC) | Cloud Run Job | Delete expired snapshots from GCS and host-local SSD |
| `host-heartbeat-check` | `*/5 * * * *` (every 5 min) | Cloud Run Job | Mark hosts with missing heartbeats as unreachable, drain, and replace |
| `lnd-channel-rebalance` | `0 */6 * * *` (every 6h) | Cloud Run Job | Trigger channel rebalancing to maintain routing liquidity |
| `lnd-backup-verify` | `30 4 * * *` (4:30am UTC) | Cloud Run Job | Verify the most recent `channel.backup` is decryptable on the secondary |

Cloud Scheduler invokes one-shot Cloud Run Jobs (not the deleted Cloud Run service). These are independent from the control plane.

---

## Monitoring & Alerting

### Dashboards

1. **Platform Overview** — active sandboxes by tier/runtime, API latency, billing msats/second, top-up rate
2. **Control Plane MIG** — instance count, CPU per instance, sandbox count per instance, request latency, peer-forward rate
3. **VM Host Pool** — per-host utilization, Firecracker count, heartbeat age, host-agent latency
4. **Lightning Node** — channel balance, pending HTLCs, payment success rate, active node identity
5. **Billing Engine** — meter ticks/sec, top-ups/sec, balance distribution histogram, paused-no-balance count
6. **Cloud SQL** — connection count, query latency P99, advisory-lock wait time

### Alert Policies

| Alert | Condition | Notification |
|---|---|---|
| LND active node unreachable | `payment_lnd_connection_status == 0` for > 1 min | PagerDuty P1 |
| Billing top-up failure rate high | `billing_topup_failures_total > 50/min` | PagerDuty P2 |
| Host pool utilization critical | Pool utilization > 90% for > 5 min | PagerDuty P2 |
| Control plane MIG error rate | HTTP 5xx rate > 5% for > 2 min | PagerDuty P2 |
| Cloud SQL latency | P99 query time > 100ms for > 5 min | Slack alert |
| LND channel balance low | `channel_balance_sats < 500_000` | Slack alert |
| GPU VM provisioning failed | `orchestrator_gpu_vm_provisioning_failed > 0` | Slack alert |
| Host-agent gRPC unreachable | `orchestrator_host_grpc_errors_total > 10/min` for any host | Slack alert; auto-drain after 5 min |

---

## IAM Service Accounts Summary

| Service Account | Used By | Key Permissions |
|---|---|---|
| `sa-control-plane` | `mig-control-plane` instances | Compute admin (host-pool MIG resize), GCS r/w on `sc-snapshots`, GCS read on `sc-templates`, Cloud SQL client, KMS decrypt, Secret accessor |
| `sa-vm-host` | Firecracker host VMs (`mig-fc-hosts`) | GCS r/w on `sc-snapshots`, GCS read on `sc-templates`, Secret accessor (host-only secrets), Monitoring writer |
| `sa-gpu-host` | Dedicated GPU VMs (created on demand) | Same as `sa-vm-host` plus GPU-specific monitoring |
| `sa-lnd` | LND HA pair VMs | GCS r/w on `sc-lnd-backups`, Secret accessor |
| `sa-cloud-build` | Cloud Build | Compute instance template admin, MIG admin, Artifact Registry writer, Cloud SQL admin |
| `sa-scheduler` | Cloud Scheduler | Cloud Run Jobs invoker |

---

## Multi-Region Plan (Phase 4)

For Phase 4, a second region (`europe-west4`) is added with mirrored infrastructure. The global LB routes agents to the nearest region by anycast. Each region has:

- Its own `mig-control-plane` (3-10 instances)
- Its own `mig-fc-hosts` and on-demand GPU pool
- Its own LND HA pair, with channels opened to the other region's pair for cross-region payment routing

Cloud SQL: a regional read replica in EU; the EU control plane writes via a multi-region wrapper that proxies writes to the primary region (acceptable for billing/audit since writes are batched). Cross-region write latency is hidden by the in-memory metering — only the audit flush crosses regions.
