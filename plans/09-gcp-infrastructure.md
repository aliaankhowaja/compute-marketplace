# Component 9: GCP Infrastructure

## Overview

This component describes the complete Google Cloud Platform infrastructure for Satoshi Compute. It covers all GCP services, VPC network design, IAM configuration, scaling policies, and operational runbooks. The infrastructure is defined as code using Terraform and applied through Cloud Build.

---

## GCP Services Used

| Service | Role |
|---|---|
| Cloud Run | Hosts the unified Go binary (control plane) + Aperture sidecar |
| Compute Engine | Firecracker host pool VMs, dedicated GPU VMs, LND Lightning node |
| Cloud Load Balancing | HTTPS termination, anycast routing to Cloud Run |
| VPC + Cloud NAT | Private networking between Cloud Run and Compute Engine VMs |
| Cloud Storage | Sandbox snapshots, Firecracker template images, Terraform state |
| Secret Manager | LND credentials, Supabase passwords, API secrets |
| Artifact Registry | Docker images for Go binary, sandbox templates |
| Cloud Build | CI/CD pipeline: build, test, migrate, deploy |
| Cloud Scheduler | Nightly reconciliation job trigger |
| Cloud Armor | DDoS protection and IP-based rate limiting |
| Cloud Monitoring | Metrics, dashboards, alerting |
| Cloud Logging | Structured log aggregation |
| IAM | Service accounts with least-privilege roles |

---

## VPC Network Design

### Network: `sc-network`

A single VPC network with 4 subnets across 2 zones for availability.

### Subnets

| Subnet Name | CIDR | Region/Zone | Purpose |
|---|---|---|---|
| `sc-control` | `10.0.0.0/24` | us-central1 | Cloud Run VPC connector, LND node |
| `sc-vm-hosts` | `10.1.0.0/20` | us-central1-a, us-central1-b | Firecracker host pool VMs |
| `sc-microvm` | `10.2.0.0/16` | N/A (virtual) | TAP interface IP space for Firecracker guests |
| `sc-gpu` | `10.3.0.0/20` | us-central1-a | Dedicated GPU VM instances |

### Cloud NAT Gateway

Two NAT gateways (one per zone for redundancy) provide outbound internet for:
- Firecracker host VMs to pull updates and sandbox images from GCS
- GPU VMs when egress is enabled for a sandbox

Sandboxes do NOT get outbound internet by default. NAT is configured with subnet exclusions and iptables on the host VM control the per-sandbox egress.

### VPC Connector for Cloud Run

Cloud Run cannot directly reach VPC resources. A VPC Serverless Connector bridges the gap:
- Name: `sc-vpc-connector`
- Subnet: `sc-control`
- CIDR: `10.0.0.128/28` (16 IPs for connector egress)
- Min instances: 2, Max instances: 10
- All Cloud Run egress traffic routed through VPC connector for private access to LND, host VMs, and Supabase private link

### Private Google Access

Enabled on all subnets. Allows VMs without external IPs to reach Google APIs (Cloud Storage, Secret Manager, Artifact Registry) without traversing the public internet.

---

## Firewall Rules

| Rule Name | Direction | Source | Destination | Ports | Purpose |
|---|---|---|---|---|---|
| `allow-internal-ssh-to-hosts` | INGRESS | `sc-control` CIDR | `sc-vm-hosts` tag | TCP 22 | Cloud Run → host VM SSH |
| `allow-host-heartbeat` | INGRESS | `sc-vm-hosts` tag | Cloud Run VPC connector | TCP 8081 | Host heartbeat to Go binary |
| `allow-lnd-grpc` | INGRESS | `sc-control` CIDR | `lnd-node` tag | TCP 10009 | Go binary → LND gRPC |
| `allow-lnd-p2p` | INGRESS | All | `lnd-node` tag | TCP 9735 | Lightning Network P2P |
| `allow-host-to-gcs` | EGRESS | `sc-vm-hosts` tag | GCS API IPs | TCP 443 | Image and snapshot access |
| `deny-microvm-to-hosts` | INGRESS | `sc-microvm` CIDR | `sc-vm-hosts` tag | All | Prevent guest-to-host breakout |
| `deny-microvm-to-microvm` | INGRESS | `sc-microvm` CIDR | `sc-microvm` CIDR | All | Prevent cross-sandbox traffic |
| `allow-gpu-egress` | EGRESS | `sc-gpu` tag + `egress-enabled` metadata | All | TCP 80, 443 | Internet access for GPU sandboxes with egress purchased |
| `deny-all-ingress` | INGRESS | All | All | All | Default deny (lowest priority) |

---

## Compute Engine: Firecracker Host Pool

### Instance Specification

| Property | Value |
|---|---|
| Machine type | `c2-standard-16` (16 vCPU, 64 GB RAM) |
| Image | Custom: `satoshi-fc-host-v1` (Ubuntu 22.04 + nested KVM license) |
| Boot disk | 100 GB SSD (pd-ssd) |
| Data disk | 500 GB SSD (pd-ssd) — for Firecracker template images and overlay disks |
| Network | `sc-vm-hosts` subnet, no external IP |
| Service account | `sa-vm-host@{project}.iam.gserviceaccount.com` |
| Tags | `vm-host`, `fc-host` |
| Metadata | `startup-script=gs://sc-scripts/fc-host-startup.sh` |

### Custom Image Build: `satoshi-fc-host-v1`

The Firecracker host image is built by Cloud Build and stored in Artifact Registry. Building involves:

1. Start from `ubuntu-2204-lts` base image
2. Enable nested virtualization via GCP license flag
3. Install: Firecracker binary (latest stable), LNDK companion, KVM kernel modules, `iproute2` (for TAP device management), `e2fsprogs` (for disk image creation)
4. Pre-download Firecracker guest kernel image and template root filesystems from GCS
5. Configure systemd service: `firecracker-agent.service` — the Go guest agent that handles vsock commands and heartbeats
6. Configure iptables rules for sandbox network isolation (loaded at boot by `sc-iptables.service`)

### Host VM Naming Convention

Pool VMs are named `vm-host-pool-{zone}-{sequential_number}`:
- `vm-host-pool-us-central1-a-001`
- `vm-host-pool-us-central1-b-001`

### Host VM Capacity

Each `c2-standard-16` host supports:
- Nano sandboxes (256 MB each): up to ~240 (leaving 4 GB for host OS and Firecracker daemons)
- Micro sandboxes (1 GB each): up to ~59
- Standard sandboxes (4 GB each): up to ~14
- Mixed workloads: bin-packing determines actual capacity

### Instance Template & Managed Instance Group

Firecracker hosts are NOT in a Managed Instance Group. They are individually managed by the Go orchestrator's autoscaler, which has richer awareness of actual microVM capacity than GCP's autoscaler. The orchestrator calls the GCP Compute Engine API directly to add/remove hosts.

---

## Compute Engine: LND Lightning Node

| Property | Value |
|---|---|
| Instance name | `lnd-node-primary` |
| Machine type | `n2-standard-4` (4 vCPU, 16 GB RAM) |
| Image | Ubuntu 22.04 LTS |
| Boot disk | 500 GB SSD |
| IP | `10.0.0.5` (static internal IP reserved) |
| External IP | Static external IP (required for Lightning P2P peer connectivity) |
| Tags | `lnd-node` |
| Service account | `sa-lnd@{project}.iam.gserviceaccount.com` |

### LND Node Setup

The LND node runs three processes managed by systemd:
1. `bitcoind` — pruned Bitcoin Core node (provides block data to LND)
2. `lnd` — Lightning Network Daemon
3. `lndk` — BOLT12 onion messaging sidecar

LND data directory: `/data/lnd/` on the 500GB boot disk.

### LND Backup Strategy

- `channel.backup` file is uploaded to GCS every 10 minutes by a cron job
- On LND restart, it checks for a newer backup in GCS before starting
- Static channel backup (SCB) allows fund recovery if the node is lost

---

## Compute Engine: GPU Pool

GPU instances are NOT pre-provisioned. They are created on-demand by the orchestrator when a gpu_small or gpu_large sandbox is requested.

### GPU Instance Specifications

| Tier | Machine Type | GPU | GPU Count | RAM |
|---|---|---|---|---|
| `gpu_small` | `n1-standard-8` | NVIDIA Tesla T4 | 1 | 30 GB |
| `gpu_large` | `a2-highgpu-1g` | NVIDIA A100 80GB | 1 | 85 GB |

### GPU Custom Image: `satoshi-gpu-cuda12-v1`

Pre-baked image stored in Artifact Registry. Contains:
- Ubuntu 22.04 LTS
- CUDA 12.2 Toolkit
- cuDNN 8.9
- NVIDIA Driver 535+
- Python 3.11, pip
- PyTorch 2.x (for ml-cuda template)
- HuggingFace Transformers, Accelerate, Datasets
- Guest agent binary
- SSH server pre-configured

Image build time: ~45 minutes. Rebuild triggered by Dockerfile changes in Cloud Build.

### GPU Instance Lifecycle

1. Orchestrator calls `compute.instances.insert` with the GPU spec
2. GCP provisions the VM (30–90 seconds)
3. Startup script: `nvidia-smi -pm 1` (enable persistence mode), start guest agent
4. Orchestrator polls `compute.instances.get` until status is `RUNNING`
5. Orchestrator records IP, SSH fingerprint in Supabase
6. On termination: `compute.instances.delete` — disks auto-deleted (configured at creation)

---

## Cloud Run Service

### Service Configuration

| Property | Value |
|---|---|
| Service name | `satoshi-compute` |
| Region | `us-central1` |
| CPU | 4 vCPU |
| Memory | 4 GiB |
| Max instances | 50 |
| Min instances | 2 (always-warm) |
| Concurrency | 1000 requests per instance |
| Request timeout | 600 seconds (for long exec calls) |
| VPC connector | `sc-vpc-connector` |
| Ingress | All (public + internal) |
| Egress | All traffic through VPC connector |

### Container Image

Built by Cloud Build and pushed to Artifact Registry as `us-central1-docker.pkg.dev/{project}/sc-images/satoshi-compute:{git_sha}`.

The image contains two processes managed by a supervisord wrapper:
1. Aperture (L402 proxy) — binds to `0.0.0.0:8080`
2. Go binary (unified backend) — binds to `127.0.0.1:8081`

### Environment Variables

All sensitive values come from Secret Manager, mounted as environment variables via Cloud Run's secret integration.

| Variable | Source |
|---|---|
| `SUPABASE_URL` | Secret Manager |
| `SUPABASE_SERVICE_KEY` | Secret Manager |
| `SUPABASE_JWT_SECRET` | Secret Manager |
| `SUPABASE_DB_HOST` | Secret Manager |
| `SUPABASE_DB_PASSWORD` | Secret Manager |
| `LND_GRPC_HOST` | `10.0.0.5:10009` (env var) |
| `LND_MACAROON_HEX` | Secret Manager |
| `LND_TLS_CERT_B64` | Secret Manager |
| `GCP_PROJECT_ID` | `sc-prod` (env var) |
| `GCP_ZONE` | `us-central1-a` (env var) |
| `GCS_SNAPSHOT_BUCKET` | `sc-snapshots` (env var) |

### Service Account: `sa-cloud-run`

Permissions granted:

| IAM Role | Scope | Purpose |
|---|---|---|
| `roles/compute.instanceAdmin.v1` | Project | Provision/delete GPU VMs and host pool VMs |
| `roles/compute.viewer` | Project | Read host VM status |
| `roles/storage.objectAdmin` | `sc-snapshots` bucket | Read/write sandbox snapshots |
| `roles/storage.objectViewer` | `sc-templates` bucket | Read Firecracker template images |
| `roles/secretmanager.secretAccessor` | Specific secrets | Read LND and DB credentials |
| `roles/monitoring.metricWriter` | Project | Write custom metrics |
| `roles/logging.logWriter` | Project | Write structured logs |
| `roles/cloudscheduler.jobRunner` | Project | Trigger reconciliation jobs |

---

## Cloud Storage Buckets

### `sc-snapshots`

| Property | Value |
|---|---|
| Location | US-CENTRAL1 (regional) |
| Storage class | STANDARD |
| Versioning | Disabled (single snapshot per sandbox, overwritten on re-pause) |
| Lifecycle rules | Delete objects > 48h old (snapshots expire after 24h; 48h gives buffer) |
| Access | Private; accessed by Cloud Run service account only |
| Encryption | Google-managed (CMEK optional for enterprise tier) |

### `sc-templates`

| Property | Value |
|---|---|
| Location | US-CENTRAL1 |
| Storage class | STANDARD |
| Content | Firecracker kernel images and root FS images |
| Access | Read by `sa-vm-host` service account; write by Cloud Build only |

### `sc-scripts`

| Property | Value |
|---|---|
| Content | Host VM startup scripts, iptables rules |
| Access | Read by `sa-vm-host`; write by Cloud Build |

### `sc-tf-state`

| Property | Value |
|---|---|
| Content | Terraform state files |
| Versioning | Enabled (for state recovery) |
| Access | Read/write by Terraform service account only |

---

## Cloud Load Balancing

### Frontend

- External HTTPS Load Balancer
- Anycast global IP
- SSL policy: `MODERN` (TLS 1.2+, strong cipher suites)
- Managed SSL certificate for `api.satoshicompute.io`

### Backend

- Backend service: Cloud Run `satoshi-compute` service
- Timeout: 600 seconds (matches Cloud Run request timeout)
- Health check: `GET /healthz` with expected 200 response

### Cloud Armor Policy

Applied to the frontend load balancer:

| Rule | Priority | Action | Description |
|---|---|---|---|
| Rate limit by IP | 1000 | Throttle (429) | > 300 requests/minute per IP |
| Block Tor exit nodes | 2000 | Deny (403) | Optional; configurable |
| Allow all | 9999 | Allow | Default allow |

---

## Secret Manager

All secrets are stored as Secret Manager secrets with automatic rotation reminders.

| Secret Name | Content | Rotation |
|---|---|---|
| `supabase-url` | Supabase project URL | Manual |
| `supabase-service-key` | Supabase service role key | 90 days |
| `supabase-jwt-secret` | JWT signing secret | 90 days |
| `supabase-db-password` | Database password | 90 days |
| `lnd-tls-cert` | LND TLS certificate PEM | On LND cert renewal |
| `lnd-invoice-macaroon` | Scoped LND macaroon | On LND restart |
| `aperture-macaroon-secret` | Aperture root key for macaroons | 180 days |

---

## Cloud Build CI/CD Pipeline

### Trigger

Any push to `main` branch in the source repository triggers the pipeline.

### Build Steps

1. **Test** — Run Go unit tests and integration tests against a Supabase test project
2. **Build** — `docker build` the unified binary image
3. **Push** — Push image to Artifact Registry with git SHA tag
4. **Migrate** — Run `supabase db push` against production Supabase (with dry-run first)
5. **Deploy** — `gcloud run deploy satoshi-compute --image {new_image}` with `--no-traffic` flag
6. **Smoke test** — Hit `/healthz` on the new revision; verify LND and DB connectivity
7. **Traffic shift** — If smoke test passes, shift 100% traffic to new revision
8. **Rollback** — If any step fails, revert to previous revision

### Cloud Build Service Account

Has `roles/run.admin`, `roles/storage.admin` (for state), `roles/compute.imageUser` (for GPU image builds).

---

## Cloud Scheduler Jobs

| Job Name | Schedule | Target | Purpose |
|---|---|---|---|
| `billing-reconciliation` | `0 2 * * *` (2am UTC) | Cloud Run Job | Find and retry unsettled billing intervals |
| `snapshot-cleanup` | `0 3 * * *` (3am UTC) | Cloud Run Job | Delete expired snapshots from GCS |
| `host-heartbeat-check` | `*/5 * * * *` (every 5 min) | Cloud Run Job | Mark hosts with missing heartbeats as unreachable |
| `lnd-channel-rebalance` | `0 */6 * * *` (every 6h) | Cloud Run Job | Trigger channel rebalancing to maintain routing liquidity |

---

## Monitoring & Alerting

### Dashboards

1. **Platform Overview** — active sandboxes by tier/runtime, API latency, billing sats/second
2. **VM Host Pool** — per-host utilization, Firecracker count, heartbeat age
3. **Lightning Node** — channel balance, pending HTLCs, payment success rate
4. **Billing Engine** — ticker count, interval settlement rate, failure rate
5. **Cloud Run** — instance count, request latency, error rate, cold start rate

### Alert Policies

| Alert | Condition | Notification |
|---|---|---|
| LND node unreachable | `payment_lnd_connection_status == 0` for > 1 min | PagerDuty P1 |
| Billing failure rate high | `billing_failures_total > 50/min` | PagerDuty P2 |
| Host pool utilization critical | Pool utilization > 90% for > 5 min | PagerDuty P2 |
| Cloud Run error rate | HTTP 5xx rate > 5% for > 2 min | PagerDuty P2 |
| Supabase DB latency | P99 query time > 500ms for > 5 min | Slack alert |
| LND channel balance low | `channel_balance_sats < 500,000` | Slack alert |
| GPU VM provisioning failed | `orchestrator_gpu_vm_provisioning_failed > 0` | Slack alert |

---

## IAM Service Accounts Summary

| Service Account | Used By | Key Permissions |
|---|---|---|
| `sa-cloud-run` | Cloud Run (Go binary) | Compute admin, GCS admin on sc-snapshots, Secret accessor |
| `sa-vm-host` | Firecracker host VMs | GCS read on sc-templates, Secret accessor (host-only secrets), Monitoring writer |
| `sa-lnd` | LND node VM | GCS read/write (channel.backup bucket), Secret accessor |
| `sa-cloud-build` | Cloud Build | Run admin, Artifact Registry writer, Compute image user |
| `sa-scheduler` | Cloud Scheduler | Cloud Run invoker |

---

## Multi-Region Plan (Phase 4)

For Phase 4, a second region (europe-west4) is added with mirrored infrastructure. The global load balancer routes agents to the nearest region by anycast.

Cross-region state: Supabase is single-region (us-east-1 or eu-west). The Go binary in the EU region connects to the same Supabase instance via Supabase's Supavisor, which supports cross-region connections with acceptable latency (typically < 30ms EU→US via Supabase's infrastructure).

LND node: one node per region, with channels opened to each other for cross-region payment routing.
