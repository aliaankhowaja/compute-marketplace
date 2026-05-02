# Component 9: Minimal GCP + Supabase + RunPod Infrastructure (MVP)

## v3 changelog

- Replaced regional GCE control-plane MIG with Cloud Run (`sockt-cp`) min=0.
- Removed Firecracker host pool MIG, internal host LB, and host-agent mTLS topology.
- Removed Cloud SQL (primary + replica + PSA) from MVP architecture.
- Replaced LND HA pair with a single small LND VM in Neutrino mode.
- Removed global external LB + Cloud Armor for MVP launch.
- Removed Cloud Build, Cloud Scheduler, Cloud NAT, and GCS snapshot pipeline.
- Added RunPod as runtime provider for CPU and GPU sandboxes.

---

## Overview

This component defines the low-cost MVP infrastructure:

- **Cloud Run** hosts the control plane only
- **RunPod** hosts all sandbox runtimes (CPU + GPU)
- **Supabase** is the only database/auth/realtime/storage backend
- **GCP Compute Engine** hosts one LND node for Lightning

The design minimizes fixed monthly spend and keeps scale-out costs usage-based.

---

## Services Used

| Service | Role |
|---|---|
| Cloud Run | Control plane API + MCP endpoints |
| Compute Engine (single VM) | LND node for Lightning operations |
| VPC (single subnet) | Private network for LND VM |
| Secret Manager | App, DB, and Lightning secrets |
| Cloud Logging + Monitoring | Basic logs/metrics/alerts |
| Supabase (external) | Postgres, Auth, Realtime, Storage |
| RunPod (external) | Sandbox compute runtime |
| Stripe (external) | Credit purchase flow |
| GitHub Actions + GHCR | Build and deploy pipeline |

Removed for MVP: Cloud SQL, Cloud Build, Cloud Scheduler, Cloud Armor, Global LB, Internal LBs, Cloud NAT, KMS SSH wrapping.

---

## Network Design

### GCP network

- VPC name: `sc-network`
- Subnet: `sc-lnd` (`10.0.0.0/24`, `us-central1`)
- Cloud Run connects to VPC using **Direct VPC egress** (no serverless connector VM costs)

### Connectivity model

- Public HTTPS from clients to Cloud Run
- Public HTTPS from Cloud Run to Supabase/RunPod/Stripe
- Private RFC1918 access from Cloud Run to LND VM over Direct VPC egress

---

## Cloud Run: `sockt-cp`

| Property | Value |
|---|---|
| Runtime | Go, Cloud Run second-gen |
| CPU / Memory | 1 vCPU, 512 MiB |
| Min instances | 0 |
| Max instances | 10 |
| Concurrency | 80 |
| Request timeout | 900s (for long MCP/stream requests) |
| Ingress | All (public) |
| Auth | Disabled at Cloud Run edge; app-level auth enforces access |
| VPC egress | Direct VPC egress to `sc-network` |

### Cost posture

- Idle cost near zero with `min_instances=0`
- Free tier handles low MVP traffic
- Increase memory only when profiling requires it

---

## Compute Engine: Single LND Node

| Property | Value |
|---|---|
| Name | `lnd-node-mvp` |
| Machine type | `e2-small` |
| Zone | `us-central1-a` |
| Disk | 30 GB `pd-balanced` |
| Internal IP | static in `10.0.0.0/24` |
| External IP | optional static if needed for inbound peer routing |
| Mode | LND Neutrino (no local bitcoind) |
| Restart policy | automatic restart |

### Services on VM

- `lnd`
- optional lightweight health sidecar exporting `/healthz`

### Backup model

- Export `channel.backup` every 5 minutes
- Upload backup artifact to Supabase Storage bucket (`lnd-backups`)

### Trade-off

This is a deliberate single point of failure for MVP cost control. Phase 2 restores active/passive HA.

---

## Supabase (External)

### MVP usage

- Postgres is the sole persistent database
- Supabase Auth for dashboard users
- Realtime for API key revocation and warning events
- Storage for small artifacts (including LND backup file)

### Connection policy

- Use Supavisor transaction pool (`:6543`) for normal request handlers
- Use Supavisor session mode (`:5432`) for operations requiring connection affinity

---

## RunPod (External)

### Runtime policy

- CPU and GPU sandboxes both run on RunPod pods
- Pod image includes `sockt-runtime`
- Pod proxy URL is returned as `data_endpoint`
- Pod stop/resume controls pause lifecycle
- Network volume optional per sandbox for persistent state

### Minimal org configuration

- One RunPod org/project
- One API key for control plane (stored in Secret Manager)
- Region allowlist for initial rollout
- Community cloud default, secure cloud as optional tier

---

## Ingress and DNS

For MVP, use:

- Cloud Run managed HTTPS endpoint
- custom domain mapping for `api.sockt.io`
- optional Cloudflare free plan in front (DNS + basic WAF)

Deferred to Phase 2:

- global anycast LB
- Cloud Armor rule engine
- HTTP/3 tuning

---

## Secrets

Store in Secret Manager:

| Secret | Purpose |
|---|---|
| `supabase-url` | Supabase base URL |
| `supabase-service-role-key` | server-side DB/auth operations |
| `runpod-api-key` | RunPod pod lifecycle calls |
| `lnd-tls-cert` | TLS for LND gRPC |
| `lnd-macaroon` | LND payment permissions |
| `wss-jwt-secret` | data plane JWT signing |
| `stripe-webhook-secret` | Stripe webhook validation |

---

## CI/CD Pipeline

Use GitHub Actions (replace Cloud Build):

1. Run tests
2. Build control-plane image
3. Build runtime pod image (`sockt-runtime`)
4. Push images to GHCR
5. Deploy Cloud Run via `gcloud run deploy`
6. Smoke-check `/healthz`

RunPod images are pulled from GHCR tags pinned by commit SHA.

---

## Operations

### Scheduled jobs (MVP)

Replace Cloud Scheduler with:

- periodic reconciliation loop inside Cloud Run (invoked by external cron ping)
- or free external cron (`cron-job.org`) calling protected admin endpoint

### Logging and alerts

Basic alert set:

- Cloud Run 5xx rate high
- LND health check failing > 1 min
- Supabase connectivity failures
- RunPod provisioning failures

---

## Cost Targets

| Area | Expected MVP Idle Cost |
|---|---|
| Cloud Run | $0-$5/mo |
| LND VM | ~$8/mo |
| Secret Manager + misc | <$1/mo |
| Total fixed infra | ~$8-$13/mo |

Variable spend:

- RunPod pod runtime + network volumes
- Stripe transaction fees

---

## Phase 2 (post-MVP)

Reintroduce v2 infra when traffic/reliability requires:

- Regional control-plane MIG
- Firecracker host pool + host-agent fabric
- Cloud SQL private HA and replicas
- Global LB + Cloud Armor
- LND HA pair
- Multi-region architecture
