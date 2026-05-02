# Component 7: MCP Server (Cloud Run MVP)

## v3 changelog

- Removed consistent-hash session affinity from MCP path.
- Removed cluster-member routing assumptions.
- MCP now runs as standard Cloud Run handlers with reconnect-friendly behavior.
- Session state minimized to allow stateless scaling.

---

## Overview

The MCP server lives inside the Cloud Run control plane and exposes Streamable HTTP transport for MCP clients and SDK proxies.

Goals for MVP:

- keep MCP available at low cost
- avoid sticky infra dependencies
- tolerate Cloud Run instance recycle via reconnect semantics

---

## Transport

- Primary: MCP Streamable HTTP
- Optional compatibility proxy: stdio bridge in `@sockt/mcp-server` package

Cloud Run settings:

- high request timeout for stream endpoints
- moderate concurrency
- min instances set to 0

---

## Session Model (MVP)

Session data should be minimal and reconstructable:

- authenticated principal
- active sandbox references
- request context metadata

If a Cloud Run instance shuts down mid-stream:

- client reconnects
- SDK/proxy replays required state
- operation resumes from latest durable sandbox state

No consistent-hash ownership is used in MVP.

---

## Tool Surface

Core MCP tools remain:

- `sandbox_create`
- `sandbox_exec`
- `sandbox_write_file`
- `sandbox_read_file`
- `sandbox_list_files`
- `sandbox_status`
- `sandbox_pause`
- `sandbox_resume`
- `sandbox_terminate`
- `sandbox_extend_balance`
- `wallet_balance`

All lifecycle calls go through control plane.
Data operations use returned `data_endpoint`.

---

## Authentication

Supported modes:

- API key
- Agent JWT
- L402 bootstrapped then JWT

MCP layer delegates auth validation to shared middleware.

---

## Error and Retry Strategy

Expected transient errors:

- `stream_interrupted`
- `instance_recycled`
- `pod_unreachable`

Client strategy:

1. refresh token if needed
2. reconnect MCP stream
3. retry idempotent tool call once

---

## Metrics

- `mcp_requests_total`
- `mcp_stream_open_total`
- `mcp_stream_interruptions_total`
- `mcp_tool_latency_ms`
- `mcp_tool_errors_total`

---

## Dependencies

- API gateway/auth middleware (Component 1)
- Orchestrator (Component 4)
- Runtime endpoints + SDK behavior (Components 8 and 10)

---

## Phase 2 (post-MVP)

If MCP concurrency grows significantly:

- add explicit sticky session strategy
- restore ownership-aware routing model
- optional dedicated MCP service tier
