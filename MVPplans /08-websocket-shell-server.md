# Component 8: WebSocket Shell Server (Runtime-Pod MVP)

## v3 changelog

- WSS termination moved to `sockt-runtime` inside RunPod pod.
- Removed control-plane-to-host balance push dependency.
- Billing events now originate from local in-pod meter.
- Host-draining flow removed for MVP runtime model.

---

## Overview

Interactive shell now connects directly to the sandbox runtime pod:

- URL: `wss://<pod_id>-8443.proxy.runpod.net/sandbox/{id}/shell?token=<data_jwt>`
- server: `internal/runtime/shell.go`
- auth: `data_jwt` verification in runtime

The control plane is not on the data path during shell streaming.

---

## Session Flow

1. SDK obtains `shell_url` and `data_jwt`.
2. Client opens WSS to runtime pod.
3. Runtime validates token (sandbox + pod + scope).
4. Runtime launches PTY session and streams frames.
5. Runtime meter emits periodic billing frames on same socket.

---

## Message Types

Client -> server:

- `input`
- `resize`
- `ping`
- `request_topup`

Server -> client:

- `stdout`
- `stderr`
- `exit`
- `billing`
- `topup`
- `low_balance`
- `out_of_balance`
- `error`

---

## Billing Fan-Out

Billing fan-out is local:

- meter tick updates local balance every second
- shell session subscribes to in-process event bus
- frames are pushed directly to connected clients

Removed MVP path:

- no gRPC `UpdateBalance` from control plane
- no host-agent relay layer

---

## Runtime Security

- reject tokens with wrong `pod_id`
- reject expired tokens
- enforce `shell` scope
- optional read-only mode for delegated sessions
- PTY runs as non-root sandbox user

---

## Failure Handling

| Failure | Behavior |
|---|---|
| token expired | close socket with `data_jwt_expired` |
| runtime restart | socket drops; SDK reconnects after refresh |
| top-up callback failure | continue metering, emit warning frames |
| out-of-balance | emit final frame, close shell after pause starts |

---

## Metrics

- `wss_connections_active`
- `wss_frame_send_latency_ms`
- `wss_disconnects_total`
- `wss_billing_frames_total`
- `wss_topup_frames_total`

---

## Dependencies

- Runtime meter (Component 3)
- Data JWT issuance (Component 1)
- SDK reconnection behavior (Component 10)

---

## Phase 2 (post-MVP)

Optional future enhancements:

- multiplexed sessions through dedicated shell gateway
- richer session recording
- HA shell relay across host migrations
