import { describe, expect, it, vi } from "vitest";

import { forwardRpcLine } from "./proxy.js";

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
