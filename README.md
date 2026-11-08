# Sockt

Sockt is a serverless compute marketplace that provisions ephemeral sandbox environments for AI agents and LLMs. It orchestrates multi-tier GPU/CPU workloads on RunPod and supports both credit-based billing and anonymous Lightning Network prepaid access, with per-second metering, WebSocket shell access, file operations, and an MCP JSON-RPC interface.

## Main services

| Service | Location | Description |
| --- | --- | --- |
| Control plane | `cmd/control-plane/` | HTTP API, MCP server, auth, sandbox orchestration, billing and payments. Deployed on Cloud Run. |
| Runtime | `cmd/sockt-runtime/` | In-sandbox command execution, file I/O, PTY shell, and metering. Runs one pod per sandbox on RunPod. |
| Pod mock | `pod-mock/` | Local RunPod/runtime emulator for development. |

## Internal packages

| Package | Purpose |
| --- | --- |
| `internal/api` | HTTP router and REST handlers |
| `internal/auth` | JWT, API key, and sandbox token auth |
| `internal/config` | Environment configuration |
| `internal/mcp` | MCP JSON-RPC implementation |
| `internal/orchestrator` | RunPod provisioning and health |
| `internal/payment` | Lightning and Stripe payments |
| `internal/runtime` | Runtime-side handlers |
| `internal/store` | PostgreSQL data access |
| `internal/lightningwallet` | Balance state helpers |

## Client SDKs

`AgentSDK/` contains the Python client (`python-sockt/`), TypeScript client (`ts-client/`), and MCP stdio proxy (`mcp-server/`).

## Quick start

```bash
cp .env.cp.example .env.cp.dev
make run      # docker compose up with control plane + pod mock
```

## Documentation

- `ARCHITECTURE.md` - system design, data flows, schema
- `API_REFERENCE.md` - HTTP API reference
- `supabase/migrations/` - database schema
