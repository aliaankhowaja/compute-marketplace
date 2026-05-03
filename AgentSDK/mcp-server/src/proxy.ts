/**
 * MCP stdio (newline-delimited JSON-RPC) → Sockt `POST /v1/mcp` Streamable HTTP proxy.
 */

import * as readline from "node:readline";

function env(name: string, fallback = ""): string {
  return (process.env[name] ?? fallback).trim();
}

export function buildAuthHeaders(): Headers {
  const h = new Headers();
  const key = env("SOCKT_API_KEY");
  const jwt = env("SOCKT_USER_JWT") || env("SOCKT_SUPABASE_JWT") || env("AGENT_JWT");
  const l402 = env("SOCKT_L402_AUTHORIZATION") || env("L402_AUTHORIZATION");
  const agentId = env("SOCKT_AGENT_ID") || env("X_AGENT_ID");

  if (key) {
    h.set("Authorization", `Bearer ${key}`);
  } else if (l402) {
    h.set("Authorization", l402);
    if (agentId) h.set("X-Agent-Id", agentId);
  } else if (jwt) {
    h.set("Authorization", `Bearer ${jwt}`);
  } else {
    throw new Error("Set SOCKT_API_KEY, SOCKT_USER_JWT/AGENT_JWT, or SOCKT_L402_AUTHORIZATION");
  }
  if (agentId && !h.has("X-Agent-Id")) {
    h.set("X-Agent-Id", agentId);
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

export async function main(): Promise<void> {
  const apiUrl = env("SOCKT_API_URL") || env("SOCKT_API_BASE_URL");
  if (!apiUrl) {
    console.error("SOCKT_API_URL (or SOCKT_API_BASE_URL) is required");
    process.exit(1);
  }
  const mcpPath = env("SOCKT_MCP_PATH", "/v1/mcp");
  const baseHeaders = buildAuthHeaders();
  let sessionId = env("SOCKT_MCP_SESSION_ID") || undefined;

  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });

  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const { bodyText, sessionId: sid } = await forwardRpcLine(trimmed, {
        apiUrl,
        mcpPath,
        sessionId,
        headers: baseHeaders,
      });
      if (sid) sessionId = sid;
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
