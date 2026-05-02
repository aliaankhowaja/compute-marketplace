# Component 8: WebSocket Shell Server

## Overview

The WebSocket Shell Server is the `internal/wss` package within the unified Go binary. It provides real-time, interactive terminal access to running sandboxes for agents and developers. It proxies stdio between the WebSocket client and the guest sandbox via virtio-vsock (Firecracker microVMs) or SSH (dedicated VMs), and injects billing event frames out-of-band so the client stays informed of payment activity.

The shell server is the lowest-latency interface to sandbox environments and the primary channel for agents that want to interactively build, test, and run code.

---

## Endpoint

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/sandbox/{id}/shell` | WebSocket upgrade. Token required as query parameter. |

### Connection URL Format

```
wss://api.satoshicompute.io/v1/sandbox/{sandbox_id}/shell?token={wss_token}
```

The `wss_token` is a short-lived (24h) bearer token returned at sandbox creation and refreshed on resume. It is not a macaroon — it is a signed JWT scoped to this specific sandbox, validated locally by the Go binary without hitting Supabase.

### wss_token Contents

| Claim | Value |
|---|---|
| `sub` | sandbox_id |
| `agent_id` | owning agent UUID |
| `iss` | satoshi-compute |
| `exp` | 24 hours from sandbox creation |
| `scope` | `shell` |

---

## Connection Lifecycle

### Phase 1: Upgrade

1. Client sends HTTP GET with `Upgrade: websocket` header
2. Go handler validates `wss_token` query parameter (signature + expiry + sandbox_id match path)
3. Handler verifies sandbox `status = 'running'` in Supabase (cached 1s)
4. WebSocket upgrade completes; Go handler spawns:
   - A goroutine reading from WebSocket client (client → sandbox)
   - A goroutine writing to WebSocket client (sandbox → client + billing frames)
   - A goroutine subscribing to billing events from the billing engine

### Phase 2: Shell Proxy

- The shell server opens a virtio-vsock connection (Firecracker) or SSH session (dedicated VM) to the guest
- The guest agent starts a PTY session inside the sandbox
- All data flowing through the proxy is raw terminal I/O (no re-framing at the pty layer)
- Billing frames are injected by the writing goroutine as JSON objects alongside the raw terminal stream

### Phase 3: Disconnection

On WebSocket close (any reason):
- The guest PTY session remains alive (sandbox keeps running)
- Billing continues unaffected
- Client can reconnect using the same `wss_token` (within its 24h validity)

On sandbox pause/terminate:
- Server sends a `paused` or `terminated` JSON frame
- Server closes the WebSocket connection with code `1001 Going Away`

---

## Message Protocol

All messages are JSON objects. The WebSocket uses text frames for JSON messages. Raw terminal output is transmitted as JSON-wrapped strings (not raw binary frames) to keep the protocol uniform and debuggable.

### Client → Server Messages

#### input
Send keystrokes or command input to the sandbox shell.

```
{
  "type": "input",
  "data": "pip install numpy\n"
}
```

- `data`: string of characters to send to the PTY stdin
- Maximum `data` length: 8192 bytes per frame
- Newlines trigger command execution in the shell

#### resize
Resize the terminal window. Should be sent whenever the client terminal dimensions change.

```
{
  "type": "resize",
  "cols": 220,
  "rows": 50
}
```

- Triggers `SIGWINCH` inside the guest PTY
- Minimum: 10 cols × 5 rows
- Maximum: 512 cols × 300 rows

#### ping
Keep-alive ping. Server responds with pong. Send every 30 seconds to prevent connection timeout.

```
{
  "type": "ping"
}
```

#### extend
Request a budget extension. Signals that the agent intends to continue the session and authorizes additional BOLT12 payment intervals.

```
{
  "type": "extend",
  "duration_secs": 300
}
```

- Server acknowledges with `extension_granted` or `extension_denied` (if BOLT12 offer is unreachable)

---

### Server → Client Messages

#### output
Terminal output from the sandbox. Contains raw terminal escape sequences (ANSI codes for color, cursor movement, etc.).

```
{
  "type": "output",
  "stream": "stdout",
  "data": "Collecting numpy...\r\n",
  "ts": "2026-04-30T12:00:10.123Z"
}
```

- `stream`: `stdout` or `stderr`
- `data`: terminal output string (may contain ANSI escape codes)
- `ts`: server-side timestamp for client-side buffering and latency measurement

#### billing
Injected after each successful billing interval settlement. Allows the client to update a live cost counter.

```
{
  "type": "billing",
  "interval_sats": 20,
  "total_consumed": 840,
  "balance_remaining": null,
  "interval_start": "2026-04-30T12:00:00Z",
  "interval_end": "2026-04-30T12:00:10Z",
  "preimage": "abc123..."
}
```

- `balance_remaining`: null for BOLT12 pull-pay agents (no pre-funded balance); integer for BOLT11/deposit agents
- `preimage`: hex preimage proving payment (agent can verify independently)

#### budget_warning
Sent when a billing payment fails and the billing engine is in warning state.

```
{
  "type": "budget_warning",
  "level": "warning",
  "consecutive_failures": 1,
  "message": "Payment failed: no routing path. Retrying next interval.",
  "pauses_at_failure": 3
}
```

- `level`: `warning` (failure 1), `critical` (failure 2), `imminent` (failure 3 = pause incoming)
- `pauses_at_failure`: how many more consecutive failures until sandbox is auto-paused

#### session_expiring
Sent when the macaroon or wss_token is approaching expiry.

```
{
  "type": "session_expiring",
  "seconds_left": 300,
  "reason": "wss_token_expiry",
  "action_required": "reconnect with refreshed token"
}
```

#### paused
Sent when the sandbox has been paused (by agent request, billing failure, or idle timeout).

```
{
  "type": "paused",
  "reason": "billing_failure",
  "snapshot_gcs": "gs://sc-snapshots/...",
  "sats_consumed_total": 840,
  "paused_at": "2026-04-30T12:01:40Z",
  "snapshot_expires_at": "2026-04-31T12:01:40Z"
}
```

- Connection closes with `1001 Going Away` after this frame

#### terminated
Sent when the sandbox has been terminated.

```
{
  "type": "terminated",
  "reason": "agent_request",
  "total_uptime_secs": 100,
  "total_sats_consumed": 840,
  "terminated_at": "2026-04-30T12:01:40Z"
}
```

- Connection closes with `1001 Going Away` after this frame

#### extension_granted
Response to an `extend` message.

```
{
  "type": "extension_granted",
  "authorized_additional_secs": 300,
  "new_expiry_at": "2026-04-30T12:06:40Z"
}
```

#### extension_denied
Response to an `extend` message when extension cannot be authorized.

```
{
  "type": "extension_denied",
  "reason": "bolt12_offer_unreachable",
  "message": "Cannot reach your Lightning node to pre-authorize extension."
}
```

#### pong
Response to client `ping`.

```
{
  "type": "pong",
  "ts": "2026-04-30T12:00:00.001Z"
}
```

---

## Shell Proxy Backends

### Backend A: virtio-vsock (Firecracker microVMs)

virtio-vsock provides a Unix socket-like interface between the Firecracker host and the guest VM without requiring TAP networking.

**Connection:** The shell server opens a vsock connection to the guest using:
- Context ID (CID): stored in `sandboxes.vsock_cid` in Supabase
- Port: 1024 (reserved for the guest shell agent)

**Session setup:**
1. Shell server sends: `{"op": "pty", "cols": 220, "rows": 50, "env": {"TERM": "xterm-256color"}}`
2. Guest agent spawns a PTY with bash/sh and responds: `{"ok": true, "pid": 1234}`
3. All subsequent data is raw PTY bytes forwarded bidirectionally

**Latency:** vsock bypasses the TAP bridge and kernel TCP/IP stack. Round-trip latency is typically < 1ms on the same physical host.

### Backend B: SSH (Dedicated VMs)

Dedicated VMs (standard overflow and GPU tiers) expose the guest agent over SSH.

**Connection:** Shell server dials `sandbox@{guest_ip}:22` using the ephemeral Ed25519 keypair stored in Supabase.

**Session setup:**
1. SSH handshake with pre-distributed host key (known at provisioning time, stored in sandbox record)
2. Shell server requests PTY channel: `ssh.Session.RequestPty("xterm-256color", rows, cols, modes)`
3. Shell server starts shell: `ssh.Session.Shell()`
4. stdin/stdout pipes connected to the WebSocket proxy goroutines

**Host key verification:** The host key fingerprint is recorded during VM provisioning and stored in `sandboxes.ssh_host_key`. The shell server verifies this fingerprint on SSH dial — no `InsecureIgnoreHostKey` allowed.

**Latency:** Slightly higher than vsock (full TCP/IP + SSH crypto overhead) — typically 2–5ms within the VPC.

---

## Idle Timeout

Sandboxes with an open WebSocket connection but no `input` messages for 30 minutes receive a `session_expiring` warning. If no activity after another 10 minutes, the server closes the WebSocket connection. The sandbox continues running (billing continues).

Sandboxes with NO open WebSocket connection for 60 minutes receive a `paused` state transition via the idle timeout detector. This is a cost-saving feature: an agent that disconnected and forgot to terminate its sandbox will have it auto-paused after 1 hour.

**Idle timeout configuration:**

| Setting | Default | Environment Variable |
|---|---|---|
| No-input warning after | 30 minutes | `WSS_IDLE_WARN_MINS` |
| No-input disconnect after | 40 minutes | `WSS_IDLE_DISCONNECT_MINS` |
| No-connection pause after | 60 minutes | `WSS_NO_CONNECTION_PAUSE_MINS` |

---

## Concurrency Limits

| Limit | Value | Enforcement |
|---|---|---|
| Max WebSocket connections per sandbox | 3 | Multiple observer connections allowed |
| Max WebSocket connections per agent | 50 | Rate limit by agent_id |
| Max input frame rate | 100 frames/second | Token bucket; excess frames dropped with `rate_limited` warning |
| Max output buffer per connection | 1 MB | If buffer fills (slow client), oldest frames dropped |

---

## Security

- wss_token is validated locally using HMAC-SHA256 with a per-sandbox secret. No Supabase round-trip per connection.
- The wss_token is single-use per connection attempt but multiple connections can use the same token within its 24h window (for reconnects).
- The shell server never logs `input` frame content (user commands). It logs connection events only.
- The vsock port 1024 on the guest is firewalled from other guests. The host can only reach a specific guest's vsock via the Firecracker API's vsock CID routing.
- SSH connections use ephemeral per-sandbox keypairs. The private key never leaves the Go binary process memory during the shell session.

---

## Billing Engine Integration

The WebSocket shell server subscribes to billing events from the billing engine via an in-process channel:

- When a billing ticker settles an interval, it sends a `BillingEvent` struct to all active WebSocket connections for that sandbox_id via the billing engine's event fan-out mechanism
- The shell server's writing goroutine receives the event and injects a `billing` JSON frame into the WebSocket stream

The fan-out is implemented as a `sync.Map` of `sandbox_id → []chan BillingEvent`. The shell server registers a channel on connection and deregisters on disconnect.

---

## Monitoring

| Metric | Type | Labels | Alert |
|---|---|---|---|
| `wss_connections_active` | Gauge | runtime | Alert if drops unexpectedly |
| `wss_connection_duration_secs` | Histogram | tier | Monitor for usage patterns |
| `wss_input_frames_total` | Counter | — | Monitor activity |
| `wss_output_bytes_total` | Counter | tier | Bandwidth monitoring |
| `wss_proxy_errors_total` | Counter | backend, error_type | Alert if vsock/ssh errors > 10/min |
| `wss_idle_pauses_total` | Counter | — | Monitor idle timeout triggers |
| `wss_billing_frames_injected_total` | Counter | — | Verify billing events reaching clients |

### Log Events

| Event | Level |
|---|---|
| `wss.connection.opened` | INFO |
| `wss.connection.closed` | INFO (with reason and duration) |
| `wss.proxy.vsock_error` | WARN |
| `wss.proxy.ssh_error` | WARN |
| `wss.idle.warning_sent` | INFO |
| `wss.idle.disconnected` | INFO |
| `wss.idle.sandbox_paused` | INFO |
| `wss.resize` | DEBUG |

---

## Dependencies

- **Orchestrator** (Component 4) — for vsock CID, guest IP, SSH key retrieval; for idle-timeout-triggered pause
- **Billing Engine** (Component 3) — subscribes to billing events for injection into WebSocket stream
- **Store Module** — for sandbox status verification and wss_token validation key lookup
- **Firecracker Guest Agent** (on host VMs) — vsock proxy target
- **GCP VPC** — private network path from Cloud Run to VM hosts for SSH sessions
