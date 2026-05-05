import { describe, expect, it, vi, afterEach } from "vitest";

import { buildAuthHeaders, forwardRpcLine } from "./proxy.js";

afterEach(() => {
  vi.unstubAllEnvs();
});

describe("buildAuthHeaders", () => {
  it("omits Authorization when no API key and no captured sandbox_token", () => {
    vi.stubEnv("SOCKT_API_KEY", "");
    const h = buildAuthHeaders();
    expect(h.has("Authorization")).toBe(false);
    expect(h.get("Content-Type")).toBe("application/json");
  });

  it("uses SOCKT_API_KEY when present", () => {
    vi.stubEnv("SOCKT_API_KEY", "sockt_live_abc");
    const h = buildAuthHeaders();
    expect(h.get("Authorization")).toBe("Bearer sockt_live_abc");
  });

  it("uses captured sandbox_token when no API key", () => {
    vi.stubEnv("SOCKT_API_KEY", "");
    const h = buildAuthHeaders({ sandboxToken: "sbx_test" });
    expect(h.get("Authorization")).toBe("Bearer sbx_test");
  });

  it("API key beats sandbox_token", () => {
    vi.stubEnv("SOCKT_API_KEY", "sockt_live_abc");
    const h = buildAuthHeaders({ sandboxToken: "sbx_test" });
    expect(h.get("Authorization")).toBe("Bearer sockt_live_abc");
  });
});

describe("forwardRpcLine", () => {
  it("posts JSON-RPC body and captures Mcp-Session-Id", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ jsonrpc: "2.0", id: 1, result: {} }), {
        status: 200,
        headers: { "Mcp-Session-Id": "sess-1" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const headers = new Headers();
    headers.set("Authorization", "Bearer test");

    const out = await forwardRpcLine(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, {
      apiUrl: "https://api.example",
      mcpPath: "/v1/mcp",
      headers,
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("https://api.example/v1/mcp");
    expect(init.method).toBe("POST");
    expect(init.body).toContain("ping");
    expect(out.sessionId).toBe("sess-1");
  });
});
