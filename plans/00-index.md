# Satoshi Compute — System Component Plans Index

## Overview

This directory contains detailed implementation plans for every major component of the Satoshi Compute platform: a pay-per-compute infrastructure service for AI agents, powered by Bitcoin Lightning micropayments via BOLT12 and L402, built on Go, GCP, and Supabase.

---

## Architecture Summary

```
Agents (Claude, Gemini, DeepSeek, GPT-4)
    │
    │  MCP tools / REST API / WebSocket
    ▼
Cloud Load Balancer (HTTPS, Cloud Armor DDoS)
    │
    ▼
Cloud Run: Unified Go Binary + Aperture Sidecar
    ├── api module         → REST handlers, router
    ├── billing module     → BOLT12 streaming payment tickers
    ├── orchestrator       → VM lifecycle (Firecracker + dedicated VMs)
    ├── payment module     → LND gRPC, BOLT12, L402
    ├── store module       → Supabase Postgres adapter
    ├── mcp module         → MCP server (tools + resources)
    └── wss module         → WebSocket shell proxy
         │
         ├── [VPC] → Firecracker Host Pool (GCP c2-standard-16, nested KVM)
         │              └── microVMs: nano, micro, (standard)
         ├── [VPC] → Dedicated GPU VMs (n1+T4, a2-highgpu-1g)
         │              └── gpu_small, gpu_large, (standard overflow)
         ├── [VPC] → LND Lightning Node (BOLT12 via LNDK)
         └── [Managed] → Supabase (Postgres, Auth, Realtime, Storage)
```

---

## Component Plans

| # | File | Component | Scope |
|---|---|---|---|
| 01 | [01-api-gateway-l402-proxy.md](./01-api-gateway-l402-proxy.md) | API Gateway & L402 Proxy | Aperture config, macaroon caveats, rate limiting, error schemas |
| 02 | [02-unified-go-binary.md](./02-unified-go-binary.md) | Unified Go Binary | Repository structure, module interfaces, all routes, handler specs |
| 03 | [03-billing-engine.md](./03-billing-engine.md) | Billing Engine | BOLT12 ticker goroutines, failure handling, reconciliation, batching |
| 04 | [04-sandbox-orchestrator.md](./04-sandbox-orchestrator.md) | Sandbox Orchestrator | Firecracker lifecycle, dedicated VM provisioning, host pool, file ops |
| 05 | [05-payment-module.md](./05-payment-module.md) | Payment Module | LND gRPC, BOLT12 offer-to-payment flow, LNDK, fallback modes |
| 06 | [06-supabase-backend.md](./06-supabase-backend.md) | Supabase Backend | Full schema, RLS policies, Auth config, Realtime, connection pooling |
| 07 | [07-mcp-server.md](./07-mcp-server.md) | MCP Server | All tools, resources, error codes, session management, client config |
| 08 | [08-websocket-shell-server.md](./08-websocket-shell-server.md) | WebSocket Shell Server | Full message protocol, vsock/SSH backends, billing injection, timeouts |
| 09 | [09-gcp-infrastructure.md](./09-gcp-infrastructure.md) | GCP Infrastructure | VPC, subnets, firewall rules, Cloud Run config, IAM, CI/CD, alerts |
| 10 | [10-agent-sdk.md](./10-agent-sdk.md) | Agent SDK | Python + TypeScript SDKs, LangChain toolkit, MCP proxy npm package |

---

## Build Order

Components should be built in this sequence, as each depends on the previous:

1. **GCP Infrastructure** (09) — provision VPC, Cloud Run, host pool, LND node
2. **Supabase Backend** (06) — apply migrations, configure RLS and Auth
3. **Payment Module** (05) — connect to LND, test BOLT12 offer validation
4. **Store Module** (part of 02) — verify Supabase connectivity and schema
5. **Sandbox Orchestrator** (04) — Firecracker host pool integration
6. **Billing Engine** (03) — BOLT12 ticker, payment flow
7. **API Gateway & L402** (01) — Aperture sidecar, macaroon issuance
8. **Unified Go Binary** (02) — wire all modules, REST routes
9. **WebSocket Shell Server** (08) — shell proxy, billing frame injection
10. **MCP Server** (07) — tool definitions, SSE transport
11. **Agent SDK** (10) — Python and TypeScript packages, MCP proxy

---

## Key Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Backend language | Go (unified binary) | In-process billing tickers; zero inter-service latency |
| Billing protocol | BOLT12 (pull-pay Offers) | Reusable offers; no per-tick invoice generation overhead |
| Auth layer | L402 + Supabase JWT | L402 for payment-gated API access; JWT for subsequent session auth |
| MicroVM technology | Firecracker | Less than 125ms boot; less than 5MB overhead; GCP nested KVM support |
| GPU isolation | Dedicated GCP VMs | Firecracker lacks GPU passthrough (PCIe work paused in 2025) |
| Database | Supabase (Postgres) | Realtime, Auth, RLS, Supavisor pooling — all in one |
| Control plane hosting | GCP Cloud Run | Zero-ops scaling; VPC connector for private VM access |
| Snapshot storage | GCP Cloud Storage | Lowest-latency access from VM hosts; lifecycle rules for auto-cleanup |
