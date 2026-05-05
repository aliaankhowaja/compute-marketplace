# Component 10: Agent SDK

## v2 changelog

- The SDK now reads `data_endpoint` and `data_jwt` from the `sandbox_create` response and uses them for **every** data-plane call (`exec`, `read_file`, `write_file`, `list_files`, `shell`). Control-plane calls (`pause`, `resume`, `terminate`, `status` summary, billing history) continue to use the main API URL.
- The SDK transparently refreshes `data_jwt` by calling `POST /v1/sandbox/{id}/refresh-data-jwt` when the JWT has < 10 min remaining or when a 401 `data_jwt_expired` is returned.
- L402 / macaroon / BOLT12 / Lexe code is removed. Auth is now strictly:
  1. `SOCKT_API_KEY=sockt_live_…` (credits, fiat users).
  2. Sandbox-scoped bearer `sbx_…` issued by `tools/call sandbox_create` (Lightning, autonomous LLMs). The MCP proxy auto-captures it; typed clients accept it via `sandbox_token` / `SOCKT_SANDBOX_TOKEN`.
  3. Supabase JWT for `/v1/user/*` only.
- Streamable HTTP MCP transport in `@sockt/mcp-server`.
- Per-sandbox WebSocket shell connects to the host-agent URL using the sandbox's `data_jwt`.
- New SDK error class `HostUnreachableError` for transient host-data-plane failures (typically resolved by `data_jwt` refresh, which the SDK does automatically with one retry before raising).

---

## Overview

The Agent SDK is a set of client libraries that abstract the Sockt API and host-data-plane discovery into a simple interface consumable by AI agents. Two packages are maintained: a **Python SDK** for Python frameworks (LangChain, LlamaIndex, custom), and a **TypeScript/JavaScript SDK** for Node.js agents. A third package — `@sockt/mcp-server` — is a stdio↔Streamable-HTTP MCP proxy used as a Cursor / Claude / Gemini connector.

The SDK's core value proposition: the agent developer does not need to understand sandbox tokens, host topology, or cluster sharding — they import the SDK, provide an API key (or let the LLM go through anonymous Lightning over MCP), and call `create_sandbox()`. The SDK handles the rest.

---

## Python SDK: `sockt`

### Installation

```
pip install sockt
```

### Core Classes

#### ComputeClient

The main entry point. Manages auth headers, REST calls, and data-plane JWT lifecycle.

**Constructor parameters:**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `api_url` | str | Yes | Sockt API base URL (e.g. `https://api.sockt.dev`) |
| `api_key` | str | No | API key for credit billing (`sockt_live_…`). Falls back to `SOCKT_API_KEY`. |
| `supabase_jwt` | str | No | Supabase user JWT for `/v1/user/*` only. Falls back to `SOCKT_SUPABASE_JWT` / `SOCKT_USER_JWT`. |
| `sandbox_token` | str | No | Sandbox-scoped bearer (`sbx_…`) for Lightning sandboxes. Falls back to `SOCKT_SANDBOX_TOKEN`. |
| `timeout_secs` | float | No | Default HTTP request timeout (default: 120) |
| `data_jwt_refresh_threshold_secs` | int | No | Refresh `data_jwt` when remaining < this (default: 600) |

**Methods:**

**`create_sandbox(billing_method, tier, **kwargs) → Sandbox`**
- REST `POST /v1/sandbox/create` (credits-only). For Lightning sandboxes, use the MCP `sandbox_create` tool — REST does not mint Lightning sandboxes.
- Returns a `Sandbox` with `data_endpoint` and a freshly minted `data_jwt`.

**`get_sandbox(sandbox_id) → Sandbox`**
- Fetches the sandbox row (including current `host_id`); mints a fresh `data_jwt` server-side via `/v1/sandbox/{id}/refresh-data-jwt`

**`list_sandboxes(status_filter=None) → List[Sandbox]`**
- Returns sandboxes for the authenticated principal

**`get_pricing() → PricingInfo`**
- Pricing without authentication

**`get_balance() → BalanceInfo`**
- Lightning agents: `balance_msats`, `total_msats_topped_up_lifetime`, `total_msats_consumed_lifetime`, `warning_level`
- Credit users: `credit_balance_usd_cents`, `total_purchased_usd_cents`, `total_consumed_usd_cents`, `warning_level`

**`get_billing_history(page=1, limit=50, sandbox_id=None) → BillingHistory`**
- Paginated billing intervals (Lightning) or credit transactions (credits)

**`get_topup_history(page=1, limit=50) → TopUpHistory`** (Lightning only)
- Paginated `top_up_intervals` records — the agent can audit Lightning settlements separately from per-tick consumption

---

#### Sandbox

Returned by `create_sandbox()` and `get_sandbox()`. Wraps the control-plane sandbox record + a per-sandbox HTTP client pinned to `data_endpoint`.

**Properties:**
- `id` (str) — sandbox UUID
- `status` (str) — current status
- `tier` (str)
- `runtime` (str)
- `template` (str)
- `host_id` (str) — current host
- `data_endpoint` (str) — host-agent base URL (refreshed on resume / 401)
- `data_jwt` (str) — current short-lived JWT (auto-refreshed)
- `data_jwt_expires_at` (datetime)
- `shell_url` (str) — derived: `data_endpoint.replace("https://","wss://") + "/shell?token=" + data_jwt`
- `sats_per_second` (float) — derived from `msats_per_second`
- `msats_per_second` (int)
- `balance_msats` (int) — last known prepaid balance
- `consumed_msats` (int)
- `created_at` (datetime)

**Methods:**

**`exec(command, timeout_ms=30000, env=None, working_dir='.') → ExecResult`**
- HTTP POST to `data_endpoint + "/exec"` with `Authorization: Bearer {data_jwt}`
- On 401 `data_jwt_expired`: refresh `data_jwt` once and retry
- On 503 `host_unreachable`: refresh (host may have moved) and retry once before raising `HostUnreachableError`

**`write_file(path, content, encoding='utf8', create_dirs=True) → WriteResult`**
- HTTP POST to `data_endpoint + "/files"`

**`read_file(path, encoding='utf8', max_bytes=10_485_760) → ReadResult`**
- HTTP GET to `data_endpoint + "/files"` with `?path=...`

**`list_files(path='.', recursive=False, max_depth=3) → List[FileEntry]`**
- HTTP GET to `data_endpoint + "/files/list"`

**`status() → SandboxStatus`**
- HTTP GET to `data_endpoint + "/status"` (live metrics + balance from the host-agent)

**`pause() → PauseResult`**
- HTTP POST to control plane: `POST {api_url}/v1/sandbox/{id}/pause`

**`resume() → ResumeResult`**
- HTTP POST to control plane: `POST {api_url}/v1/sandbox/{id}/resume`
- On success, the SDK updates `data_endpoint` and mints a fresh `data_jwt` (the response includes both)

**`terminate() → TerminateResult`**
- HTTP DELETE on control plane: `DELETE {api_url}/v1/sandbox/{id}`

**`extend_balance(cents=None) → ExtendBalanceResult`**
- HTTP POST to control plane: `POST {api_url}/v1/sandbox/{id}/extend-balance`
- Credits-only. For Lightning sandboxes, the LLM tops up by paying the BOLT11 surfaced in `sandbox_status.pending_invoice` over MCP — there is no REST equivalent.

**`shell() → ShellSession`**
- Opens an interactive WebSocket shell session (returns a context manager)
- Connects directly to `shell_url`

**`wait_until_running(timeout_secs=120, poll_interval_secs=2) → None`**
- Blocks until `status == 'running'` (relevant for GPU tiers with long cold starts)

**`refresh_data_jwt() → None`**
- Manual refresh of the data-plane credentials. The SDK calls this automatically when needed; exposed for callers that want explicit control.

---

#### ShellSession

Context manager for interactive shell access. Used with `with sandbox.shell() as shell:`.

**Methods:**

**`send(text)`**, **`send_line(text)`**, **`recv(timeout_secs=10)`**, **`recv_until(pattern, timeout_secs=30)`**, **`resize(cols, rows)`**, **`wait_for_prompt(timeout_secs=10)`**, **`request_topup(sats)`**, **`close()`**

**`on_billing_event(callback)`** — register a callback for `billing` and `topup` frames.
**`on_topup_failed(callback)`** — register a callback for `topup_failed` warnings.
**`on_low_balance(callback)`** — register a callback for `low_balance` warnings.
**`on_host_draining(callback)`** — register a callback so the SDK can transparently reconnect (the default callback does so automatically).

The default `on_host_draining` handler:
1. Waits `reconnect_after_ms` from the message
2. Refreshes `data_jwt` (which gives the new `host_id` if the sandbox migrated)
3. Reconnects the WebSocket to the new `shell_url`
4. Replays the last `resize` event to the new connection

---

### LangChain Integration

The Python SDK provides a `SocktToolkit` compatible with LangChain agents. Tools exposed:
- `sockt_create_sandbox`
- `sockt_exec_command`
- `sockt_write_file`
- `sockt_read_file`
- `sockt_list_files`
- `sockt_sandbox_status`
- `sockt_pause_sandbox`
- `sockt_resume_sandbox`
- `sockt_terminate_sandbox`
- `sockt_extend_balance`

Each tool has a detailed description written for LLM consumption. The toolkit handles auth, top-ups, host migration, and `data_jwt` refresh transparently.

---

### Error Classes

| Class | When raised |
|---|---|
| `AuthenticationError` | API key / sandbox token / JWT invalid or missing |
| `SandboxNotFoundError` | Sandbox ID not found or belongs to another principal |
| `SandboxNotRunningError` | Sandbox is paused or terminated |
| `InsufficientBalanceError` | Initial top-up failed (Lightning routing failure) or credits exhausted |
| `TopUpFailedError` | Mid-session top-up failed; raised by `extend_balance()` |
| `NoCapacityError` | No Firecracker hosts available; GPU provisioning failed |
| `ExecTimeoutError` | Command exceeded `timeout_ms` |
| `FileTooLargeError` | File exceeds 50MB (use chunked API) |
| `RateLimitError` | Too many requests; includes `retry_after_secs` |
| `HostUnreachableError` | host-agent unreachable after refresh+retry — caller may retry |
| `APIError` | Generic API error with `status_code` and `error_slug` |

---

### Auth handling

The SDK supports three authentication paths:

1. **API key (credits, recommended for fiat users):** `SOCKT_API_KEY=sockt_live_…` — sent as `Authorization: Bearer …` on every REST and MCP call. New sandboxes default to `billing_method=credits`.
2. **Sandbox token (Lightning, autonomous LLMs):** `Authorization: Bearer sbx_…` produced by `tools/call sandbox_create`. The MCP proxy (`@sockt/mcp-server`) auto-captures it from the JSON-RPC result and reuses it on subsequent requests; the typed clients (`python-sockt`, `@sockt/client`) accept it via the `sandbox_token` constructor option or `SOCKT_SANDBOX_TOKEN`.
3. **Supabase JWT:** only required for `/v1/user/*` endpoints (profile, credits ledger, API key management). Not used for sandbox operations.

The SDKs no longer ship Lexe / BOLT12 / macaroon code paths — paying a Lightning invoice is the responsibility of whatever wallet the LLM already has (e.g. the `lnbot` MCP). The SDK's only job is to surface the BOLT11 it sees on `sandbox_create` / `pending_invoice` and to keep using the `sandbox_token` until the sandbox is paused or terminated.

---

### Automatic JWT Refresh

`data_jwt` expires in < 10 minutes (configurable): the SDK silently calls `refresh_data_jwt()` against the control plane and updates the `Sandbox` object in place. If refresh fails the SDK raises `AuthenticationError`.

---

### Host Migration Handling

When a sandbox is paused and resumed on a different host (cold restore from GCS):
- `resume()` returns the new `data_endpoint` and a fresh `data_jwt`; the SDK updates the `Sandbox` object in place
- Any in-flight `exec`/`write_file`/`read_file` calls (rare, since the SDK doesn't normally call into a paused sandbox) get a one-shot retry against the new endpoint
- An open `ShellSession` receives `{"type":"host_draining"}`, the default handler reconnects to the new URL automatically

---

## TypeScript/JavaScript SDK: `@sockt/client`

### Installation

```
npm install @sockt/client
```

### Core API

The TypeScript SDK mirrors the Python SDK with idiomatic TypeScript patterns (async/await, generics, type safety).

#### ComputeClient

```typescript
interface ComputeClientOptions {
  apiUrl: string;
  apiKey?: string;
  supabaseJwt?: string;
  sandboxToken?: string;
  timeoutMs?: number;
  dataJwtRefreshThresholdSecs?: number;  // default 600
}
```

**Methods:**

- `createSandbox(options: { billingMethod: 'credits'; tier: string }): Promise<Sandbox>` — REST, credits-only.
- `getSandbox(sandboxId: string): Promise<Sandbox>`
- `getPricing(): Promise<unknown[]>`

#### Sandbox

```typescript
class Sandbox {
  // Properties
  id: string;
  status: SandboxStatus;
  tier: Tier;
  runtime: 'firecracker' | 'dedicated_vm';
  template: string;
  hostId: string;
  dataEndpoint: string;
  dataJwt: string;
  dataJwtExpiresAt: Date;
  shellUrl: string;
  satsPerSecond: number;
  msatsPerSecond: number;
  balanceMsats: number;
  consumedMsats: number;
  createdAt: Date;

  // Methods
  exec(command: string, options?: ExecOptions): Promise<ExecResult>;
  writeFile(path: string, content: string, options?: WriteOptions): Promise<WriteResult>;
  readFile(path: string, options?: ReadOptions): Promise<ReadResult>;
  listFiles(path?: string, options?: ListOptions): Promise<FileEntry[]>;
  runtimeStatus(): Promise<Record<string, unknown>>;
  pause(): Promise<void>;
  resume(): Promise<void>;
  terminate(): Promise<void>;
  extendBalance(opts?: { cents?: number }): Promise<Record<string, unknown>>;
  shell(): ShellSession;
  waitUntilRunning(options?: WaitOptions): Promise<void>;
  refreshDataJwt(): Promise<void>;
}
```

#### ShellSession

EventEmitter-based.

**Events emitted:**
- `output` — `{stream: 'stdout'|'stderr', data: string, ts: string}`
- `billing` — `BillingEvent`
- `topup` — `TopupEvent`
- `topup_failed` — `TopupFailureEvent`
- `low_balance` — `LowBalanceEvent`
- `out_of_balance` — `OutOfBalanceEvent`
- `host_draining` — `HostDrainingEvent` (default handler auto-reconnects)
- `paused` — `PausedEvent`
- `terminated` — `TerminatedEvent`
- `error` — Error
- `close` — connection closed

**Methods:**
- `send(text: string): void`
- `sendLine(text: string): void`
- `resize(cols: number, rows: number): void`
- `requestTopup(sats: number): void`
- `close(): void`
- `recvUntil(pattern: RegExp, timeoutMs?: number): Promise<string>`

---

### MCP Server npm Package: `@sockt/mcp-server`

A standalone npm package that runs as an MCP server process, bridging the MCP stdio transport to the Sockt MCP **Streamable HTTP** transport.

**Purpose:** Allows Claude Desktop and other stdio-only MCP clients to connect to Sockt without any integration code.

**Usage:**

```
npx @sockt/mcp-server
```

**Environment variables consumed:**

| Variable | Description |
|---|---|
| `SOCKT_API_URL` | API base URL |
| `SOCKT_API_KEY` | Optional API key for credits sandboxes (`sockt_live_…`) |
| `SOCKT_SANDBOX_TOKEN` | Optional pre-set sandbox bearer (`sbx_…`) — usually captured automatically |
| `SOCKT_MCP_PATH` | Override `/v1/mcp` if the control plane is mounted elsewhere |
| `LOG_LEVEL` | debug, info, warn, error |

**Tools exposed:** Identical to the MCP server tools defined in Component 7 (`sandbox_create`, `sandbox_exec`, `sandbox_write_file`, `sandbox_read_file`, `sandbox_list_files`, `sandbox_status`, `sandbox_pause`, `sandbox_resume`, `sandbox_terminate`).

**Transport:** MCP Streamable HTTP to the platform; stdio to the local MCP client.

**Auth handling:** If `SOCKT_API_KEY` is set, the proxy sends `Authorization: Bearer sockt_live_…` on every request. Otherwise it goes anonymous: `initialize` and `tools/list` work; `tools/call sandbox_create` returns an `awaiting_payment` result with `sandbox_token=sbx_…`, which the proxy captures from `result.structuredContent.sandbox_token` and reuses on every subsequent request as `Authorization: Bearer sbx_…`. The proxy also surfaces top-up `pending_invoice` blocks to stderr so a human watching the logs can see what to pay.

---

## SDK Testing Infrastructure

### Test Mode

Both SDKs support a test mode that points to a Sockt testnet deployment:
- `api_url = "https://test.sockt.dev"`
- `LIGHTNING_NETWORK=testnet` on the control plane
- Testnet sats; testnet sandboxes have lower resource limits but identical API

### Integration Test Suite

The SDK repository includes integration tests against the testnet:
- `test_create_and_exec.py` — Create nano sandbox, run a Python script, verify output (uses `data_endpoint` directly)
- `test_file_operations.py` — Write and read files via host-agent
- `test_billing.py` — Verify billing events received via WSS, including `topup` and `low_balance`
- `test_pause_resume.py` — Full pause/resume lifecycle, including host-migration scenario
- `test_data_jwt_refresh.py` — Force `data_jwt` to expire and verify SDK refresh
- `test_host_draining.py` — Drain a host mid-session; verify automatic reconnection
- `test_mcp_tools.py` — All MCP tools via the MCP proxy package
- `test_langchain_toolkit.py` — LangChain tool integration

---

## Documentation

The SDK includes:
- **Quickstart guide** — 5-minute tutorial: pick API key (credits) or anonymous Lightning, create sandbox, run code, terminate.
- **Authentication guide** — API key vs sandbox token vs Supabase JWT.
- **Tier selection guide** — Which tier for which workload, cost estimates.
- **Billing guide** — Sandbox-scoped prepaid balance + threshold top-up; how to interpret `low_balance`, `out_of_balance`, and `pending_invoice`.
- **Host topology guide** — What `data_endpoint` means; when the SDK refreshes it.
- **LangChain integration guide** — Setting up the toolkit.
- **Claude / Cursor MCP guide** — `claude_desktop_config.json` / `mcp.json` setup.
- **API reference** — Auto-generated from docstrings and TypeScript types.

---

## Publishing & Versioning

| Package | Registry | Versioning |
|---|---|---|
| `sockt` (Python) | PyPI | Semantic versioning; `2.x` line for the new architecture |
| `@sockt/client` (TypeScript) | npm | Semantic versioning; `2.x` line |
| `@sockt/mcp-server` (TypeScript) | npm | Mirrors client version |

### Release Process

1. Tag `v{major}.{minor}.{patch}` in the SDK repository
2. Cloud Build pipeline runs SDK tests against testnet
3. If tests pass: publish to PyPI and npm
4. Create GitHub release with changelog

---

## Dependencies

- **Control-plane API** (Component 2) — auth, sandbox lifecycle, `data_jwt` minting
- **Host-agent HTTPS data plane** (Components 4, 8) — exec, files, list, status, shell — reached via `data_endpoint`
- **MCP Server** (Component 7) — `@sockt/mcp-server` proxies stdio MCP to the Streamable HTTP transport and auto-captures the sandbox bearer
- **Supabase Auth** — JWT issuance for `/v1/user/*` (profile, credits, API keys); the SDK accepts an existing JWT and includes it in headers
