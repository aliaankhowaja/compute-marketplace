# Component 10: Agent SDK

## v2 changelog

- The SDK now reads `data_endpoint` and `data_jwt` from the `sandbox_create` response and uses them for **every** data-plane call (`exec`, `read_file`, `write_file`, `list_files`, `shell`). Control-plane calls (`pause`, `resume`, `terminate`, `status` summary, billing history) continue to use the main API URL.
- The SDK transparently refreshes `data_jwt` (and the underlying `data_endpoint`, in case the sandbox migrated to a new host) by calling `POST /v1/sandbox/{id}/refresh-data-jwt` when the JWT has < 10 min remaining or when a 401 `data_jwt_expired` is returned.
- The L402 retry loop is replaced with **one-shot top-up handling**: the SDK pays one initial L402 challenge at registration time and then never sees a 402 again. All subsequent funding flows through the platform's prepaid balance + threshold top-up; the SDK observes `low_balance` events on the WSS but does not perform L402 payments per call.
- Streamable HTTP MCP transport replaces SSE in the npm `@sockt/mcp-server` package (lockstep with Component 7).
- Per-sandbox WebSocket shell connects directly to the host-agent URL (`wss://api.sockt.dev/h/{host_id}/sandbox/{id}/shell?token={data_jwt}`).
- New SDK error class `HostUnreachableError` for transient host-data-plane failures (typically resolved by `data_jwt` refresh, which the SDK does automatically with one retry before raising).

---

## Overview

The Agent SDK is a set of client libraries that abstract the Sockt API, the L402 onboarding flow, BOLT12 wallet integration, and host-data-plane discovery into a simple interface consumable by AI agents. Two packages are maintained: a **Python SDK** for agents built with Python frameworks (LangChain, LlamaIndex, custom), and a **TypeScript/JavaScript SDK** for Node.js-based agents.

The SDK's core value proposition: the agent developer does not need to understand L402, BOLT12, macaroons, top-ups, host topology, or cluster sharding — they import the SDK, provide a Lexe seed (or API key), and call `create_sandbox()`. The SDK handles the rest.

---

## Python SDK: `sockt`

### Installation

```
pip install sockt
```

### Core Classes

#### ComputeClient

The main entry point. Handles L402 onboarding, JWT management, control-plane API calls.

**Constructor parameters:**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `api_url` | str | Yes | Sockt API base URL (e.g. `https://api.sockt.dev`) |
| `bolt12_offer` | str | No | Pre-registered BOLT12 offer string. Skips registration when present. |
| `lexe_seed_path` | str | No | Path to Lexe seed file. Required if `bolt12_offer` is not pre-registered. |
| `lexe_env` | str | No | `mainnet` or `testnet` (default: mainnet) |
| `lightning_address` | str | No | Alternative for BOLT11 manual top-up mode |
| `api_key` | str | No | Alternative auth: API key for credit billing |
| `jwt` | str | No | Pre-existing agent JWT |
| `jwt_cache_path` | str | No | File path to persist the agent JWT across process restarts |
| `timeout_secs` | int | No | Default HTTP request timeout (default: 30) |
| `data_jwt_refresh_threshold_secs` | int | No | Refresh `data_jwt` when remaining < this (default: 600) |
| `log_level` | str | No | `debug`, `info`, `warn`, `error` (default: info) |

**Methods:**

**`register() → AgentInfo`**
- Validates or registers the agent's BOLT12 offer with the platform
- Handles the **one-time** L402 challenge: detect 402, pay invoice via Lexe, retry
- Returns `AgentInfo(agent_id, jwt, jwt_expires_at, balance_msats)`
- Idempotent

**`create_sandbox(tier, template=None, duration_secs=None, egress=False, label=None, idempotency_key=None) → Sandbox`**
- Provisions a sandbox; returns a `Sandbox` object that already knows its `data_endpoint` and `data_jwt`
- Triggers initial top-up automatically
- Generates `idempotency_key` if not provided

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

**`exec(command, timeout_ms=30000, env=None, working_dir='/workspace') → ExecResult`**
- HTTP POST to `data_endpoint + "/exec"` with `Authorization: Bearer {data_jwt}`
- On 401 `data_jwt_expired`: refresh `data_jwt` once and retry
- On 503 `host_unreachable`: refresh (host may have moved) and retry once before raising `HostUnreachableError`

**`write_file(path, content, encoding='utf8', create_dirs=True) → WriteResult`**
- HTTP POST to `data_endpoint + "/files"`

**`read_file(path, encoding='utf8', max_bytes=10_485_760) → ReadResult`**
- HTTP GET to `data_endpoint + "/files"` with `?path=...`

**`list_files(path='/workspace', recursive=False, max_depth=3) → List[FileEntry]`**
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

**`extend_balance(sats, idempotency_key=None) → ExtendBalanceResult`**
- HTTP POST to control plane: `POST {api_url}/v1/sandbox/{id}/extend-balance`
- Pre-funds the sandbox with `sats` worth of additional balance via a synchronous BOLT12 top-up (Lightning) or credit allocation (credits). Useful before a known-large workload.

**`delegate(readonly=False, expiry_secs=3600, max_exec_calls=None) → str`**
- Returns an attenuated macaroon for sub-agent delegation (control-plane call)

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
| `AuthenticationError` | L402 onboarding failed; Lexe wallet unreachable; JWT invalid |
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

### One-Shot L402 Onboarding in the SDK

The SDK handles L402 once at registration time, then never again:

1. On `register()`, the SDK calls `POST /v1/wallet/offer`
2. If the response is 402: extract the BOLT11 invoice from `WWW-Authenticate`, pay via the Lexe SDK, retry with `Authorization: L402 <macaroon>:<preimage>`
3. The 200 response carries the agent JWT, which the SDK caches
4. For all subsequent API calls, the SDK uses `Authorization: Bearer <jwt>` — no further L402 challenges
5. When balance falls below threshold the **platform** triggers BOLT12 pull-pay top-ups against the agent's offer; the SDK is informed via WSS `topup` / `topup_failed` frames but does not initiate Lightning calls itself

This eliminates the per-call L402 retry loop from v1.

---

### Automatic JWT Refresh

The SDK monitors JWT expiry and refreshes:
- Agent JWT expires in < 1 hour: call `register()` to obtain a fresh one (Lexe re-pays the 1-sat L402 challenge if needed)
- `data_jwt` expires in < 10 minutes (configurable): silently call `refresh_data_jwt()` against the control plane

If refresh fails: raise `AuthenticationError` with a clear message.

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
  bolt12Offer?: string;
  lexeSeedPath?: string;
  lexeEnv?: 'mainnet' | 'testnet';
  lightningAddress?: string;
  apiKey?: string;
  jwt?: string;
  jwtCachePath?: string;
  timeoutMs?: number;
  dataJwtRefreshThresholdSecs?: number;  // default 600
  logLevel?: 'debug' | 'info' | 'warn' | 'error';
}
```

**Methods:**

- `register(): Promise<AgentInfo>`
- `createSandbox(options: CreateSandboxOptions): Promise<Sandbox>`
- `getSandbox(sandboxId: string): Promise<Sandbox>`
- `listSandboxes(filter?: StatusFilter): Promise<Sandbox[]>`
- `getPricing(): Promise<PricingInfo>`
- `getBalance(): Promise<BalanceInfo>`
- `getBillingHistory(options?: BillingHistoryOptions): Promise<BillingHistory>`
- `getTopupHistory(options?: TopupHistoryOptions): Promise<TopupHistory>`

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
  status(): Promise<SandboxStatus>;
  pause(): Promise<PauseResult>;
  resume(): Promise<ResumeResult>;
  terminate(): Promise<TerminateResult>;
  extendBalance(sats: number, idempotencyKey?: string): Promise<ExtendBalanceResult>;
  delegate(options?: DelegateOptions): Promise<string>;
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
| `SOCKT_API_KEY` | API key for credit billing (or...) |
| `AGENT_JWT` | Pre-existing agent JWT for L402 path |
| `BOLT12_OFFER` | Agent's BOLT12 offer (for auto-registration) |
| `LEXE_SEED_PATH` | Path to Lexe seed file (one-time L402 onboarding only) |
| `LOG_LEVEL` | debug, info, warn, error |

**Tools exposed:** Identical to the MCP server tools defined in Component 7 (`sandbox_create`, `sandbox_exec`, `sandbox_write_file`, `sandbox_read_file`, `sandbox_list_files`, `sandbox_status`, `sandbox_pause`, `sandbox_resume`, `sandbox_terminate`, `sandbox_extend_balance`, `wallet_balance`, `sandbox_delegate`).

**Transport:** MCP Streamable HTTP to the platform; stdio to the local MCP client.

**Auth handling:** The proxy uses the TypeScript SDK internally. On startup, if `AGENT_JWT` is set, that's used directly. If `BOLT12_OFFER` and `LEXE_SEED_PATH` are set, the SDK performs the one-time L402 onboarding to obtain a JWT and caches it. If `SOCKT_API_KEY` is set, the proxy uses Bearer auth and skips L402 entirely.

---

## SDK Testing Infrastructure

### Test Mode

Both SDKs support a test mode that points to a Sockt testnet deployment:
- `api_url = "https://test.sockt.dev"`
- `lexe_env = "testnet"`
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
- **Quickstart guide** — 5-minute tutorial: register, create sandbox, run code, terminate
- **Authentication guide** — BOLT12 offer setup with Lexe; Lightning address fallback; API key path
- **Tier selection guide** — Which tier for which workload, cost estimates
- **Billing guide** — Prepaid balance + auto top-up explained; how to interpret `low_balance` and `topup_failed` events
- **Host topology guide** — What `data_endpoint` means; when the SDK refreshes it
- **LangChain integration guide** — Setting up the toolkit
- **Claude MCP guide** — `claude_desktop_config.json` setup
- **Sub-agent delegation guide** — Macaroon attenuation
- **API reference** — Auto-generated from docstrings and TypeScript types

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

- **Control-plane API** (Component 2) — auth, sandbox lifecycle, billing history, `data_jwt` minting
- **Host-agent HTTPS data plane** (Components 4, 8) — exec, files, list, status, shell — reached via `data_endpoint`
- **MCP Server** (Component 7) — `@sockt/mcp-server` proxies stdio MCP to the Streamable HTTP transport
- **Lexe SDK** — Python `lexe` PyPI package; TypeScript `@lexe/client` npm package (used for one-shot L402 onboarding only)
- **Supabase Auth** — JWT issuance for website-user API-key flows; SDK accepts an existing JWT and includes it in headers
