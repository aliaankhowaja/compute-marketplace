/**
 * MCP stdio (newline-delimited JSON-RPC) → Sockt `POST /v1/mcp` Streamable HTTP proxy.
 *
 * Auth model:
 *   - SOCKT_API_KEY=sockt_live_…  ⇒  fiat / credits user. Sent as `Authorization: Bearer …`.
 *   - Otherwise anonymous lightning. The proxy auto-captures `sandbox_token` (sbx_…)
 *     from any `sandbox_create` result and reuses it on subsequent calls.
 */

import * as readline from "node:readline";

function env(name: string, fallback = ""): string {
  return (process.env[name] ?? fallback).trim();
}

export type ProxyState = {
  sessionId?: string;
  sandboxToken?: string;
};

export function buildAuthHeaders(state?: ProxyState): Headers {
  const h = new Headers();
  const key = env("SOCKT_API_KEY");
  if (key) {
    h.set("Authorization", `Bearer ${key}`);
  } else if (state?.sandboxToken) {
    h.set("Authorization", `Bearer ${state.sandboxToken}`);
  }
  h.set("Content-Type", "application/json");
  h.set("Accept", "application/json");
  return h;
}

export async function forwardRpcLine(
  line: string,
  opts: { apiUrl: string; mcpPath: string; sessionId?: string; headers: Headers },
): Promise<{ bodyText: string; sessionId?: string }> {
  const base = opts.apiUrl.replace(/\/+$/, "");
  const path = opts.mcpPath.startsWith("/") ? opts.mcpPath : `/${opts.mcpPath}`;
  const url = `${base}${path}`;
  const headers = new Headers(opts.headers);
  if (opts.sessionId) {
    headers.set("Mcp-Session-Id", opts.sessionId);
  }
  const ctrl = new AbortController();
  const timeoutMs = Number(env("SOCKT_HTTP_TIMEOUT_MS", "120000"));
  const t = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const resp = await fetch(url, {
      method: "POST",
      headers,
      body: line,
      signal: ctrl.signal,
    });
    const sid = resp.headers.get("mcp-session-id") || resp.headers.get("Mcp-Session-Id") || opts.sessionId;
    const bodyText = await resp.text();
    return { bodyText, sessionId: sid || undefined };
  } finally {
    clearTimeout(t);
  }
}

export function extractRpcId(line: string): unknown {
  try {
    const obj = JSON.parse(line) as { id?: unknown };
    return obj.id ?? null;
  } catch {
    return null;
  }
}

type StructuredContent = {
  sandbox_token?: string;
  sandbox_id?: string;
  invoice?: string;
  amount_msats?: number;
  amount_sats?: number;
  pending_invoice?: { bolt11?: string; amount_msats?: number };
  next_step?: string;
  status?: string;
};

type RpcResponse = {
  result?: { isError?: boolean; structuredContent?: StructuredContent };
};

function captureSandboxToken(state: ProxyState, sc: StructuredContent | undefined) {
  if (!sc) return;
  const t = typeof sc.sandbox_token === "string" ? sc.sandbox_token.trim() : "";
  if (t.startsWith("sbx_")) {
    state.sandboxToken = t;
  }
}

function surfaceHints(j: RpcResponse | undefined) {
  const sc = j?.result?.structuredContent;
  if (!sc) return;
  if (typeof sc.invoice === "string" && sc.invoice.startsWith("ln")) {
    const sats = sc.amount_sats ?? (sc.amount_msats ? Math.ceil(sc.amount_msats / 1000) : undefined);
    const note = sats ? ` (~${sats} sat)` : "";
    console.error(`[sockt-mcp] Sandbox awaiting payment${note}. Pay BOLT11: ${sc.invoice}`);
    if (sc.next_step) console.error(`[sockt-mcp] Next: ${sc.next_step}`);
  }
  const pi = sc.pending_invoice;
  if (pi && typeof pi.bolt11 === "string" && pi.bolt11.startsWith("ln")) {
    const sats = pi.amount_msats ? Math.ceil(pi.amount_msats / 1000) : undefined;
    const note = sats ? ` (~${sats} sat)` : "";
    console.error(`[sockt-mcp] Top-up invoice pending${note}. Pay BOLT11: ${pi.bolt11}`);
  }
}

export async function main(): Promise<void> {
  const apiUrl = env("SOCKT_API_URL") || env("SOCKT_API_BASE_URL");
  if (!apiUrl) {
    console.error("SOCKT_API_URL (or SOCKT_API_BASE_URL) is required");
    process.exit(1);
  }
  const mcpPath = env("SOCKT_MCP_PATH", "/v1/mcp");
  const state: ProxyState = {
    sessionId: env("SOCKT_MCP_SESSION_ID") || undefined,
    sandboxToken: env("SOCKT_SANDBOX_TOKEN") || undefined,
  };

  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });

  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const { bodyText, sessionId: sid } = await forwardRpcLine(trimmed, {
        apiUrl,
        mcpPath,
        sessionId: state.sessionId,
        headers: buildAuthHeaders(state),
      });
      if (sid) state.sessionId = sid;
      try {
        const j = JSON.parse(bodyText) as RpcResponse;
        captureSandboxToken(state, j?.result?.structuredContent);
        surfaceHints(j);
      } catch {
        /* not JSON */
      }
      process.stdout.write(bodyText + "\n");
    } catch (e) {
      const errObj = {
        jsonrpc: "2.0",
        id: extractRpcId(trimmed),
        error: { code: -32603, message: String(e) },
      };
      process.stdout.write(JSON.stringify(errObj) + "\n");
    }
  }
}
