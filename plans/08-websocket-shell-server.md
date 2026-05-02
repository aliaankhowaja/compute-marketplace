# Component 8: WebSocket Shell Server

## v2 changelog

- The WSS endpoint moves entirely off the control plane and onto the **host-agent** running on each host VM. WSS connections terminate on the same VM that runs the sandbox; the control plane is no longer in the data path.
- The agent connects to a host-aware URL: `wss://api.sockt.io/h/{host_id}/sandbox/{id}/shell`. The global LB routes by path prefix to the internal `ilb-host-data`, which routes to the named host's `:8443`.
- The control plane mints `wss_token` (a `data_jwt` with `scope=shell`) at sandbox creation/resume time. The host-agent verifies the JWT locally — no control-plane round-trip.
- Billing-event fan-out is now an **in-process channel inside the host-agent**. The control plane pushes `BalanceUpdate` to the host-agent via the gRPC `UpdateBalance` RPC; the host-agent broadcasts to local WSS clients. No cross-instance fan-out is needed because the WSS terminates on the host that owns the sandbox.
- `wss_token` schema unified with `data_jwt` (Component 1); shell connections also accept the broader `data_jwt` so the SDK can use one credential for `/exec` and `/shell`.
- vsock backend goes via the host-agent's local connection; the kernel TCP stack is bypassed entirely. SSH backend on dedicated VMs is removed in favour of in-process exec via cgroups + namespaces (consistent with Component 4).

---

## Overview

The WebSocket Shell Server is the `internal/hostagent/shell.go` module within the **host-agent** binary. It provides real-time, interactive terminal access to running sandboxes for agents and developers.

It proxies stdio between the WebSocket client and the guest sandbox via virtio-vsock (Firecracker microVMs) or local pipes (dedicated GPU VMs), and injects billing event frames out-of-band so the client stays informed of payment activity.

Because the shell server runs on the host that owns the sandbox, the data path is:

```
Agent → GLB → ilb-host-data → host-agent :8443 → vsock → guest PTY
```

There is no Cloud Run, no VPC connector, no Aperture, no extra control-plane hop. End-to-end interactive latency is dominated by Internet RTT to the agent; in-VPC server-side latency is < 5ms typical.

---

## Endpoint

| Method | Path | Description |
|---|---|---|
| `GET` | `/h/{host_id}/sandbox/{id}/shell` | WebSocket upgrade. Token required as query parameter. |

(Internally, the global LB strips the `/h/{host_id}` prefix before forwarding; the host-agent sees `GET /sandbox/{id}/shell`.)

### Connection URL Format

```
wss://api.sockt.io/h/{host_id}/sandbox/{sandbox_id}/shell?token={data_jwt}
```

The `{host_id}` is returned by the control plane in `sandbox.create` (or refreshed via `/v1/sandbox/{id}/refresh-data-jwt`). The SDK reads `data_endpoint` from the create response and uses it directly.

### `data_jwt` Contents (used as `wss_token`)

| Claim | Value |
|---|---|
| `iss` | `sockt-cp` |
| `aud` | `host-data-plane` |
| `sub` | `sandbox_id` |
| `host_id` | the host VM the sandbox lives on |
| `principal_kind` | `l402` or `api_key` |
| `agent_id` or `user_id` | the owner |
| `iat` | issuance time |
| `exp` | iat + 1 hour |
| `scope` | array including `shell` |
| `readonly` | true if delegated readonly |

Signed with HS256 using `wss-jwt-secret` from Secret Manager. The host-agent loads the same secret at startup. Verification is constant-time HMAC + scope check; no control-plane round-trip.

---

## Connection Lifecycle

### Phase 1: Upgrade

1. Client sends HTTP GET with `Upgrade: websocket` to `:8443` (TLS terminated by `ilb-host-data` upstream)
2. host-agent validates the `data_jwt`:
   - HMAC signature using local `wss-jwt-secret`
   - `exp` not exceeded
   - `host_id` matches the local `HOST_ID`
   - `sub` matches the path's `{id}`
   - `scope` includes `shell`
   - `data_jwt_revoked_at` (cached from a periodic Cloud SQL read every 30s) is unset
3. host-agent looks up the sandbox in its local in-memory sandbox table — must be present and `running`
4. WebSocket upgrade completes; host-agent spawns:
   - A goroutine reading from the WebSocket client (client → guest)
   - A goroutine writing to the WebSocket client (guest → client + billing frames)
   - A goroutine subscribed to the local billing-event channel for this sandbox

### Phase 2: Shell Proxy

- Firecracker: open a virtio-vsock connection to the guest using the sandbox's `vsock_cid`, port 1024
- Dedicated VM: spawn a PTY-attached process group locally inside the host-agent's process under user `sandbox` (UID 1000) with appropriate namespaces and cgroups
- The guest agent (Firecracker) starts a PTY session; the host-agent forwards bytes
- Billing frames are injected by the writing goroutine as JSON objects alongside the raw terminal stream

### Phase 3: Disconnection

On WebSocket close (any reason):
- The guest PTY session remains alive (sandbox keeps running)
- Billing continues unaffected
- Client can reconnect using the same `data_jwt` (within its 1h validity); the SDK proactively refreshes via `/v1/sandbox/{id}/refresh-data-jwt`

On sandbox pause/terminate (signaled by control plane via gRPC `PauseSandbox`/`TerminateSandbox`):
- Server sends a `paused` or `terminated` JSON frame to all open WSS clients
- Server closes the WebSocket connection with code `1001 Going Away`

On host drain (preparing for MIG scale-in or rolling update):
- Server sends `{"type":"host_draining","reconnect_after_ms":1000}` to all clients
- Closes with `1001 Going Away`
- Clients reconnect after refreshing `data_jwt` (which may now point at a different host because the sandbox migrated)

---

## Message Protocol

All messages are JSON objects. The WebSocket uses text frames.

### Client → Server Messages

#### input

```
{ "type": "input", "data": "pip install numpy\n" }
```

- Maximum `data` length: 8192 bytes per frame
- Newlines trigger command execution

#### resize

```
{ "type": "resize", "cols": 220, "rows": 50 }
```

- Triggers `SIGWINCH` inside the guest PTY
- Bounds: 10×5 to 512×300

#### ping

```
{ "type": "ping" }
```

- Server responds with `pong`. Clients should send every 30s.

#### request_topup

(New, replaces the old `extend` in semantics for the prepaid model.)

```
{ "type": "request_topup", "sats": 600 }
```

- Forwards to the control plane via gRPC: the host-agent calls `control_plane.RequestTopUp(sandbox_id, sats)` which invokes `payment.TopUpBalance`
- Server acknowledges with `topup_started`, then `topup` (on success) or `topup_failed`

---

### Server → Client Messages

#### output

```
{ "type": "output", "stream": "stdout", "data": "Collecting numpy...\r\n", "ts": "2026-04-30T12:00:10.123Z" }
```

#### billing

(New shape — uses msats and reflects the in-memory balance model.)

```
{
  "type": "billing",
  "billing_method": "lightning",
  "interval_msats": 2000,
  "balance_msats_remaining": 4_320_000,
  "consumed_msats_total": 6_840_000,
  "metered_at": "2026-04-30T12:00:10Z"
}
```

For credits sandboxes:

```
{
  "type": "billing",
  "billing_method": "credits",
  "interval_usd_cents_x100": 28,
  "balance_remaining_usd_cents": 4000,
  "metered_at": "2026-04-30T12:00:10Z"
}
```

#### topup

(Sent when a Lightning top-up settles.)

```
{
  "type": "topup",
  "amount_sats": 600,
  "balance_msats_after": 4_920_000,
  "preimage": "abc...",
  "settled_at": "ISO8601"
}
```

#### topup_started / topup_failed

```
{ "type": "topup_started", "amount_sats": 600 }
{ "type": "topup_failed", "reason": "no_route", "consecutive_failures": 1 }
```

#### low_balance / out_of_balance

```
{ "type": "low_balance", "balance_msats": 8_000, "estimated_seconds_remaining": 4 }
{ "type": "out_of_balance", "consecutive_topup_failures": 3, "paused_at": "ISO8601" }
```

(Credits use `low_credits` and `out_of_credits` analogously.)

#### budget_warning (deprecated, kept for compatibility)

Replaced by `topup_failed` (per attempt) and `out_of_balance` (final). The legacy frame is sent for one major-version overlap; SDK consumers should switch to the new frames.

#### session_expiring

```
{ "type": "session_expiring", "seconds_left": 300, "reason": "data_jwt_expiry", "action_required": "refresh data_jwt and reconnect" }
```

#### paused / terminated / host_draining

```
{ "type": "paused", "reason": "billing_failure", "snapshot_storage": "host_local_ssd", "msats_consumed_total": 6_840_000, "paused_at": "ISO8601" }
{ "type": "terminated", "reason": "agent_request", "total_uptime_secs": 100, "msats_consumed_total": 6_840_000, "terminated_at": "ISO8601" }
{ "type": "host_draining", "reconnect_after_ms": 1000, "reason": "rolling_update | scale_in" }
```

Connection closes with `1001 Going Away` after these.

#### pong

```
{ "type": "pong", "ts": "2026-04-30T12:00:00.001Z" }
```

---

## Shell Proxy Backends

### Backend A: virtio-vsock (Firecracker microVMs)

virtio-vsock provides a Unix-socket-like interface between the Firecracker host and the guest VM without TAP networking.

**Connection:** The host-agent opens a vsock connection to the guest:
- Context ID (CID): from the local in-memory sandbox table (populated when `host_agent.CreateSandbox` returned)
- Port: 1024 (reserved for the guest shell agent)

**Session setup:**
1. host-agent sends: `{"op":"pty","cols":220,"rows":50,"env":{"TERM":"xterm-256color"}}`
2. Guest agent spawns a PTY with bash/sh and responds: `{"ok":true,"pid":1234}`
3. All subsequent data is raw PTY bytes forwarded bidirectionally

**Latency:** vsock bypasses TAP and the kernel TCP stack. Round-trip latency is typically < 1ms on the same physical host.

### Backend B: Local PTY (Dedicated GPU/Standard VMs)

Dedicated VMs run the host-agent directly; there is no separate sandbox VM. The shell session is a local PTY inside the host-agent's process tree, isolated by user/cgroup/namespace.

**Session setup:**
1. host-agent forks `/bin/bash` under user `sandbox` (UID 1000) with `setsid`, attaches a PTY pair (forkpty)
2. PTY master fd is read by the WSS write goroutine; PTY slave is the bash stdin/stdout/stderr
3. cgroups limits are pre-configured at sandbox creation time (CPU shares, memory, GPU access)

**Latency:** Sub-1ms (in-process pipe).

(SSH is no longer used as a routine RPC channel. See Component 4 for the operator-only SSH backdoor.)

---

## Idle Timeout

Sandboxes with an open WebSocket connection but no `input` messages for 30 minutes receive a `session_expiring` warning. If no activity after another 10 minutes, the host-agent closes the WebSocket. The sandbox continues running (billing continues) — the WSS close is a UI-level idle, not a sandbox lifecycle event.

Sandboxes with NO open WebSocket connection for 60 minutes are NOT auto-paused by the host-agent; that decision belongs to the control plane (which has the full picture across multiple instances). The host-agent simply notifies the control plane via the heartbeat stream that this sandbox has had no WSS connection for 60+ min; the control plane invokes `orchestrator.PauseSandbox` if appropriate.

**Idle timeout configuration (host-agent env):**

| Setting | Default | Environment Variable |
|---|---|---|
| No-input warning after | 30 minutes | `WSS_IDLE_WARN_MINS` |
| No-input disconnect after | 40 minutes | `WSS_IDLE_DISCONNECT_MINS` |
| No-connection report after | 60 minutes | `WSS_NO_CONNECTION_REPORT_MINS` |

---

## Concurrency Limits

| Limit | Value | Enforcement |
|---|---|---|
| Max WebSocket connections per sandbox | 3 | host-agent local (multiple observer connections allowed) |
| Max WebSocket connections per agent (per host) | 50 | host-agent local |
| Max input frame rate | 100 frames/second | Token bucket; excess frames dropped with `rate_limited` warning |
| Max output buffer per connection | 1 MB | If buffer fills (slow client), oldest frames dropped |

Cross-host per-agent limits are enforced by the control plane via the rate-limit middleware on the `/v1/sandbox/{id}/refresh-data-jwt` endpoint (refresh attempts are throttled).

---

## Security

- `wss_token` (the `data_jwt`) is validated locally using HMAC-SHA256 with a shared secret loaded from Secret Manager at host-agent startup. No DB round-trip per connection.
- The token is **not** single-use — multiple connections can use the same token within its 1h validity (for reconnects and observer sessions).
- The host-agent **never** logs `input` frame content (user commands). It logs connection events only (open, close, idle disconnect, error).
- The vsock port 1024 on the guest is firewalled from other guests at the host's iptables layer.
- The host can only reach a specific guest's vsock via the Firecracker-assigned CID; cross-CID access is impossible by Firecracker design.
- Per-sandbox PTY processes on dedicated VMs run with `no_new_privs` and a seccomp filter; even a sandbox with kernel-bug-free root cannot escape its cgroup or namespace.

---

## Billing Engine Integration (Local, In-Process)

The host-agent maintains a local mirror of each sandbox's balance, fed by `UpdateBalance` gRPC pushes from the control plane (Component 3 → host-agent gRPC at `:7443`):

```
control plane billing engine  --gRPC UpdateBalance-->  host-agent
                                                           │
                                                           ├─ updates internal balance cache
                                                           └─ sends BillingEvent on the local fan-out channel
                                                                  │
                                                                  └─ fan-out goroutine writes to all connected WSS clients for sandbox_id
```

The fan-out is implemented as a `sync.Map[sandbox_id] → []chan BillingEvent` inside the host-agent. The shell server registers a channel on connection and deregisters on disconnect. Capacity per channel: 100; if a slow consumer fills the channel, the oldest event is dropped (we send the current cumulative state on the next push, so transient drops are harmless).

The control plane pushes `UpdateBalance` once per metering tick (default 1s). If the host-agent is briefly unreachable (e.g., during failover or restart), the next push contains the latest cumulative state and the WSS clients see a single jump rather than missing intervals.

---

## Monitoring

| Metric | Type | Labels | Alert |
|---|---|---|---|
| `ha_wss_connections_active` | Gauge | runtime | Alert if drops unexpectedly |
| `ha_wss_connection_duration_secs` | Histogram | tier | Monitor for usage patterns |
| `ha_wss_input_frames_total` | Counter | — | Monitor activity |
| `ha_wss_output_bytes_total` | Counter | tier | Bandwidth monitoring |
| `ha_wss_proxy_errors_total` | Counter | backend, error_type | Alert if vsock errors > 10/min |
| `ha_wss_idle_disconnects_total` | Counter | — | Monitor idle behaviour |
| `ha_wss_billing_frames_injected_total` | Counter | — | Verify billing events reach clients |
| `ha_wss_jwt_verify_failures_total` | Counter | reason | Alert if > 10/min (possible attack) |

### Log Events

| Event | Level |
|---|---|
| `ha.wss.opened` | INFO |
| `ha.wss.closed` | INFO (with reason and duration) |
| `ha.wss.proxy.vsock_error` | WARN |
| `ha.wss.proxy.pty_error` | WARN (dedicated VMs) |
| `ha.wss.idle.warning_sent` | INFO |
| `ha.wss.idle.disconnected` | INFO |
| `ha.wss.host_draining_sent` | INFO |
| `ha.wss.resize` | DEBUG |
| `ha.wss.jwt_invalid` | WARN |

---

## Dependencies

- **Orchestrator** (Component 4) — provides `vsock_cid`, sandbox lifecycle events to host-agent
- **Billing Engine** (Component 3) — pushes `UpdateBalance` via gRPC; host-agent fans out to WSS
- **Native L402 + JWT Module** (Component 1) — mints `data_jwt` (the `wss_token`); host-agent verifies locally
- **GCP Internal LB `ilb-host-data`** (Component 9) — routes `/h/{host_id}/...` traffic to this host-agent's `:8443`
- **Firecracker Guest Agent** (on host VMs) — vsock proxy target
- **GCP Secret Manager** — `wss-jwt-secret` shared with control plane
