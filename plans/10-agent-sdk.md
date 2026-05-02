# Component 10: Agent SDK

## Overview

The Agent SDK is a set of client libraries that abstract the Satoshi Compute API, L402 payment flow, and BOLT12 wallet interactions into a simple interface consumable by AI agents. Two packages are maintained: a **Python SDK** for agents built with Python frameworks (LangChain, LlamaIndex, custom), and a **TypeScript/JavaScript SDK** for Node.js-based agents.

The SDK's core value proposition is: the agent developer does not need to understand L402, BOLT12, macaroons, or Lightning — they import the SDK, provide a Lexe seed or BOLT12 offer, and call `create_sandbox()`.

---

## Python SDK: `satoshi-compute`

### Installation

Published to PyPI as `satoshi-compute`.

```
pip install satoshi-compute
```

### Core Classes

#### ComputeClient

The main entry point. Handles L402 authentication, BOLT12 payment, JWT management, and all API calls.

**Constructor parameters:**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `api_url` | str | Yes | Satoshi Compute API base URL |
| `bolt12_offer` | str | No | Pre-registered BOLT12 offer string. If provided, skips registration flow. |
| `lexe_seed_path` | str | No | Path to Lexe wallet seed file. Required if bolt12_offer not pre-registered. |
| `lexe_env` | str | No | `mainnet` or `testnet` (default: mainnet) |
| `lightning_address` | str | No | Alternative: Lightning address for BOLT11 fallback |
| `jwt` | str | No | Pre-existing agent JWT. If valid, skips registration. |
| `jwt_cache_path` | str | No | File path to cache the agent JWT across process restarts |
| `timeout_secs` | int | No | Default HTTP request timeout (default: 30) |
| `log_level` | str | No | `debug`, `info`, `warn`, `error` (default: info) |

**Methods:**

**`register() → AgentInfo`**
- Validates or registers the agent's BOLT12 offer with the platform
- Handles L402 authentication: detects 402 response, pays invoice via Lexe, retries
- Returns `AgentInfo` with `agent_id`, `jwt`, `jwt_expires_at`
- Called automatically by other methods if not already registered
- Idempotent: safe to call multiple times

**`create_sandbox(tier, template=None, duration_secs=None, egress=False, label=None) → Sandbox`**
- Provisions a sandbox and returns a `Sandbox` object
- Triggers BOLT12 billing automatically; no manual payment needed
- Parameters mirror the REST API parameters

**`get_sandbox(sandbox_id) → Sandbox`**
- Fetches a `Sandbox` object for an existing sandbox

**`list_sandboxes(status_filter=None) → List[Sandbox]`**
- Returns all sandboxes for the authenticated agent
- `status_filter`: optional — `running`, `paused`, `terminated`

**`get_pricing() → PricingInfo`**
- Returns current tier pricing without requiring authentication

**`get_balance() → BalanceInfo`**
- Returns agent wallet and billing summary

**`get_billing_history(page=1, limit=50, sandbox_id=None) → BillingHistory`**
- Returns paginated billing interval history

---

#### Sandbox

Returned by `create_sandbox()` and `get_sandbox()`. Represents a live sandbox session.

**Properties:**
- `id` (str) — sandbox UUID
- `status` (str) — current status
- `tier` (str) — compute tier
- `runtime` (str) — firecracker or dedicated_vm
- `template` (str) — template used
- `wss_url` (str) — WebSocket URL
- `wss_token` (str) — WebSocket token
- `sats_per_second` (float) — billing rate
- `sats_consumed` (int) — total sats billed so far
- `created_at` (datetime)

**Methods:**

**`exec(command, timeout_ms=30000, env=None, working_dir='/workspace') → ExecResult`**
- Executes a command synchronously in the sandbox
- Returns `ExecResult` with `stdout`, `stderr`, `exit_code`, `duration_ms`, `timed_out`

**`write_file(path, content, encoding='utf8', create_dirs=True) → WriteResult`**
- Writes a file to the sandbox filesystem

**`read_file(path, encoding='utf8', max_bytes=10485760) → ReadResult`**
- Reads a file from the sandbox filesystem
- Returns `ReadResult` with `content`, `size_bytes`, `truncated`

**`list_files(path='/workspace', recursive=False, max_depth=3) → List[FileEntry]`**
- Lists files and directories

**`status() → SandboxStatus`**
- Fetches current status including CPU, memory, sats consumed

**`pause() → PauseResult`**
- Pauses the sandbox and stops billing

**`resume() → ResumeResult`**
- Resumes a paused sandbox

**`terminate() → TerminateResult`**
- Permanently terminates the sandbox

**`delegate(readonly=False, expiry_secs=3600, max_exec_calls=None) → str`**
- Returns an attenuated macaroon for sub-agent delegation

**`shell() → ShellSession`**
- Opens an interactive WebSocket shell session (returns a context manager)

**`wait_until_running(timeout_secs=120, poll_interval_secs=2) → None`**
- Blocks until `status == 'running'` (for GPU tiers with long cold starts)

---

#### ShellSession

Context manager for interactive shell access. Used with `with sandbox.shell() as shell:`.

**Methods:**

**`send(text) → None`**
- Sends input to the shell

**`send_line(text) → None`**
- Sends text followed by newline (triggers command execution)

**`recv(timeout_secs=10) → str`**
- Receives output from the shell, blocks until output available or timeout

**`recv_until(pattern, timeout_secs=30) → str`**
- Receives output until a regex pattern matches (e.g., wait for shell prompt)
- Returns all output received up to and including the matched pattern

**`resize(cols, rows) → None`**
- Resizes the terminal

**`wait_for_prompt(timeout_secs=10) → None`**
- Waits for a shell prompt (`$` or `#` pattern)

**`on_billing_event(callback) → None`**
- Registers a callback for billing events: `callback(BillingEvent)`

**`close() → None`**
- Closes the shell session (sandbox remains running)

---

### LangChain Integration

The Python SDK provides a `SatoshiComputeToolkit` compatible with LangChain agents:

**Available tools exposed to LangChain:**
- `satoshi_create_sandbox` — Create a sandbox with specified tier and template
- `satoshi_exec_command` — Execute a command in a sandbox by ID
- `satoshi_write_file` — Write a file to a sandbox
- `satoshi_read_file` — Read a file from a sandbox
- `satoshi_list_files` — List files in a sandbox directory
- `satoshi_sandbox_status` — Get status and billing info
- `satoshi_pause_sandbox` — Pause a sandbox
- `satoshi_terminate_sandbox` — Terminate a sandbox

Each tool has a detailed description written for LLM consumption, explaining when to use it and what it returns. The toolkit handles authentication transparently.

---

### Error Classes

| Class | When raised |
|---|---|
| `AuthenticationError` | L402 payment failed; Lexe wallet unreachable |
| `SandboxNotFoundError` | Sandbox ID not found or belongs to another agent |
| `SandboxNotRunningError` | Sandbox is paused or terminated |
| `InsufficientBalanceError` | BOLT12 offer cannot cover estimated cost |
| `NoCapacityError` | No Firecracker hosts available; GPU provisioning failed |
| `ExecTimeoutError` | Command exceeded timeout_ms |
| `FileTooLargeError` | File exceeds 50MB (use chunked API instead) |
| `RateLimitError` | Too many requests; includes `retry_after_secs` |
| `APIError` | Generic API error with `status_code` and `error_slug` |

---

### L402 Auto-Payment in the SDK

The SDK transparently handles L402 authentication:

1. On first request, the SDK detects HTTP 402 response
2. Extracts the BOLT12 invoice from `WWW-Authenticate` header
3. Pays the invoice using the Lexe SDK: `lexe_wallet.pay_invoice(invoice)`
4. Retries the original request with `Authorization: L402 {macaroon}:{preimage}`
5. Caches the macaroon in memory and (optionally) on disk for reuse
6. If the macaroon expires, automatically re-pays for a new one

This flow is invisible to the agent developer.

---

### Automatic JWT Refresh

The SDK monitors JWT expiry and refreshes the token proactively:
- If JWT expires in < 1 hour: automatically call the registration endpoint to get a fresh JWT
- If refresh fails: raise `AuthenticationError` with clear message

---

## TypeScript/JavaScript SDK: `@satoshi-compute/client`

### Installation

Published to npm.

```
npm install @satoshi-compute/client
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
  jwt?: string;
  jwtCachePath?: string;
  timeoutMs?: number;
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

#### Sandbox

**Methods:**

- `exec(command: string, options?: ExecOptions): Promise<ExecResult>`
- `writeFile(path: string, content: string, options?: WriteOptions): Promise<WriteResult>`
- `readFile(path: string, options?: ReadOptions): Promise<ReadResult>`
- `listFiles(path?: string, options?: ListOptions): Promise<FileEntry[]>`
- `status(): Promise<SandboxStatus>`
- `pause(): Promise<PauseResult>`
- `resume(): Promise<ResumeResult>`
- `terminate(): Promise<TerminateResult>`
- `delegate(options?: DelegateOptions): Promise<string>`
- `shell(): ShellSession`
- `waitUntilRunning(options?: WaitOptions): Promise<void>`

#### ShellSession

Returns an EventEmitter-based object for the WebSocket shell.

**Events emitted:**
- `output` — `{stream: 'stdout'|'stderr', data: string}`
- `billing` — `BillingEvent`
- `budget_warning` — `BudgetWarning`
- `paused` — `PausedEvent`
- `terminated` — `TerminatedEvent`
- `error` — Error object
- `close` — connection closed

**Methods:**
- `send(text: string): void`
- `sendLine(text: string): void`
- `resize(cols: number, rows: number): void`
- `close(): void`
- `recvUntil(pattern: RegExp, timeoutMs?: number): Promise<string>`

---

### MCP Server npm Package: `@satoshi-compute/mcp-server`

A standalone npm package that runs as an MCP server process, bridging MCP stdio transport to the Satoshi Compute HTTP API.

**Purpose:** Allows Claude Desktop and other MCP clients to connect to Satoshi Compute without the agent developer writing any integration code.

**Usage:**

```
npx @satoshi-compute/mcp-server
```

**Environment variables consumed:**

| Variable | Description |
|---|---|
| `SATOSHI_API_URL` | API base URL |
| `AGENT_JWT` | Pre-existing agent JWT (if already registered) |
| `BOLT12_OFFER` | Agent's BOLT12 offer (for auto-registration) |
| `LEXE_SEED_PATH` | Path to Lexe seed file (for L402 auto-payment) |
| `LOG_LEVEL` | debug, info, warn, error |

**Tools exposed:** Identical to the MCP server tools defined in Component 7.

**Transport:** Stdio (compatible with Claude Desktop `command` transport).

**L402 Handling:** The MCP proxy package uses the TypeScript SDK's L402 auto-payment logic. When a tool call requires payment, the proxy pays the invoice via Lexe and retries transparently.

---

## SDK Testing Infrastructure

### Test Mode

Both SDKs support a test mode that points to a Satoshi Compute testnet deployment:
- `api_url = "https://test.satoshicompute.io"`
- `lexe_env = "testnet"`
- Uses testnet sats (no real value)
- Testnet sandboxes have lower resource limits but identical API

### Integration Test Suite

The SDK repository includes integration tests that run against the testnet:
- `test_create_and_exec.py` — Create nano sandbox, run a Python script, verify output
- `test_file_operations.py` — Write and read files
- `test_billing.py` — Verify billing events received via WebSocket
- `test_pause_resume.py` — Full pause/resume lifecycle
- `test_mcp_tools.py` — All MCP tools via the MCP proxy package
- `test_langchain_toolkit.py` — LangChain tool integration

---

## Documentation

The SDK includes:
- **Quickstart guide** — 5-minute tutorial: register, create sandbox, run code, terminate
- **Authentication guide** — BOLT12 offer setup with Lexe, Lightning address fallback
- **Tier selection guide** — Which tier for which workload, cost estimates
- **LangChain integration guide** — Setting up the toolkit in a LangChain agent
- **Claude MCP guide** — claude_desktop_config.json setup
- **Sub-agent delegation guide** — Using macaroon attenuation for multi-agent workflows
- **API reference** — Auto-generated from docstrings and TypeScript types

---

## Publishing & Versioning

| Package | Registry | Versioning |
|---|---|---|
| `satoshi-compute` (Python) | PyPI | Semantic versioning; `1.x` stable |
| `@satoshi-compute/client` (TypeScript) | npm | Semantic versioning |
| `@satoshi-compute/mcp-server` (TypeScript) | npm | Mirrors client version |

### Release Process

1. Tag `v{major}.{minor}.{patch}` in the SDK repository
2. Cloud Build pipeline runs SDK tests against testnet
3. If tests pass: publish to PyPI and npm
4. Create GitHub release with changelog

---

## Dependencies

- **Unified Go Binary API** (Component 2) — all SDK calls target the REST API
- **MCP Server** (Component 7) — MCP server package communicates with `/mcp/sse` and `/mcp/message` endpoints
- **Lexe SDK** — Python: `lexe` PyPI package; TypeScript: `@lexe/client` npm package
- **Supabase Auth** — JWT validation on the server side; SDK receives JWT and includes in headers
