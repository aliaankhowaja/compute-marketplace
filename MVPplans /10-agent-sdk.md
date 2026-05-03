# Component 10: Agent SDK (RunPod MVP)

## v3 changelog

- `data_endpoint` now points to RunPod proxy URL.
- Runtime enum changed to `runpod_cpu | runpod_gpu`.
- Removed host-draining specific reconnect flow.
- Kept automatic `data_jwt` refresh.
- Updated transient errors to RunPod-centric failure modes.

---

## Overview

SDK responsibilities remain the same:

- simplify auth and onboarding
- manage sandbox lifecycle
- expose exec/files/shell primitives
- hide token refresh and retries

But data-plane routing now targets runtime pods directly via RunPod URLs.

---

## Core Endpoint Model

Control-plane API:

- `https://api.sockt.dev/v1/...`

Data-plane per sandbox:

- `https://<pod_id>-8443.proxy.runpod.net/sandbox/{sandbox_id}`

Shell URL:

- `wss://<pod_id>-8443.proxy.runpod.net/sandbox/{sandbox_id}/shell?token=<data_jwt>`

---

## Runtime Enum

Replace old runtime values with:

- `runpod_cpu`
- `runpod_gpu`

Expose in SDK `Sandbox.runtime`.

---

## Automatic Refresh Behavior

The SDK refreshes `data_jwt` when:

- token has <10 minutes remaining
- runtime returns 401 token-expired
- runtime endpoint changed after resume/redeploy

Refresh call:

- `POST /v1/sandbox/{id}/refresh-data-jwt`

On refresh, SDK updates:

- `data_jwt`
- `data_jwt_expires_at`
- `data_endpoint` (if pod replaced)
- `shell_url`

---

## Method Behavior

`exec`, `read_file`, `write_file`, `list_files`, `status`:

- hit current `data_endpoint`
- one refresh+retry on auth errors
- one retry on transient `pod_starting` responses

`pause`, `resume`, `terminate`:

- control-plane lifecycle calls
- resume may return new pod id and endpoint

---

## Error Classes (MVP)

| Error | Meaning |
|---|---|
| `HostUnreachableError` | runtime endpoint not reachable after retry |
| `NoCapacityError` | RunPod cannot place requested tier in selected region |
| `SandboxNotRunningError` | operation attempted on paused/terminated sandbox |
| `TopUpFailedError` | manual top-up action failed |
| `AuthenticationError` | JWT/API key/L402 issue |

Keep `HostUnreachableError` name for compatibility even though backend is pod-based.

---

## Python + TypeScript Notes

Both SDKs should:

- surface `pod_id` in sandbox metadata
- cache current endpoint per sandbox instance
- avoid assumptions about stable host identity
- remain transport-compatible with MCP proxy package

---

## MCP Proxy Package

`@sockt/mcp-server` behavior is unchanged from caller perspective:

- uses SDK internals
- follows updated `data_endpoint` model
- retries stream/tool calls on reconnect-safe failures

---

## Testing Focus

Update integration tests for:

- endpoint format includes RunPod proxy host
- runtime enum values updated
- resume flow returning new pod id
- token refresh path on pod restart
- no host-draining event assumptions

---

## Dependencies

- Control-plane auth + lifecycle APIs (Components 1 and 2)
- Runtime WSS/data plane (Component 8)
- MCP server transport (Component 7)

---

## Phase 2 (post-MVP)

If architecture migrates back to host-based runtime:

- keep SDK abstraction stable
- adapt endpoint discovery internals only
- maintain backward-compatible error mapping
