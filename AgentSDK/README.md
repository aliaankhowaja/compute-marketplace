# Sockt Agent SDK (Phase 6)

Monorepo-local SDKs for the Sockt control plane and RunPod-backed runtime.

## Layout

| Path | Package |
|------|---------|
| `python-sockt/` | Python `sockt` client |
| `ts-client/` | npm `@sockt/client` |
| `mcp-server/` | npm `@sockt/mcp-server` (stdio → Streamable HTTP) |
| `tests/` | Integration-style tests (mocked HTTP; no live RunPod by default) |

**Python:** On PEP 668–managed systems, use `python3 -m venv .venv && .venv/bin/pip install -e ./python-sockt`.

**TypeScript / Python:** Data-plane `GET /sandbox/{id}/status` is `sandbox.runtimeStatus()` / `sandbox.runtime_status()` — keep separate from the control-plane lifecycle string on `sandbox.status`.

## Quick start

1. Copy [`AgentSDK/.env.example`](./.env.example) to `.env` and fill values.
2. Python: `pip install -e ./python-sockt` then use `sockt.ComputeClient`.
3. TypeScript: `cd ts-client && npm install && npm run build`.
4. MCP proxy: `cd mcp-server && npm install && npm run build && node dist/index.js`.

See `.env.example` for every supported variable and comments.

## Tests

```bash
cd AgentSDK/tests
python -m pytest -q
```

```bash
cd AgentSDK/ts-client && npm test
cd AgentSDK/mcp-server && npm test
```
