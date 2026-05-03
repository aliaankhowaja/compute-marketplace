import { describe, expect, it } from "vitest";

import {
  deriveDataEndpoint,
  parsePodIdFromRunpodEndpoint,
  runpodProxyBase,
  shellUrlFor,
} from "./util.js";

describe("runpod endpoints", () => {
  it("runpodProxyBase matches documented shape", () => {
    expect(runpodProxyBase("abc123")).toBe("https://abc123-8443.proxy.runpod.net");
  });

  it("deriveDataEndpoint prefers explicit proxy URL", () => {
    expect(deriveDataEndpoint("https://custom.example/pod", "ignored")).toBe("https://custom.example/pod");
  });

  it("deriveDataEndpoint falls back to pod id", () => {
    expect(deriveDataEndpoint("", "xyz")).toBe("https://xyz-8443.proxy.runpod.net");
  });

  it("parsePodIdFromRunpodEndpoint round-trip", () => {
    const base = runpodProxyBase("podA");
    expect(parsePodIdFromRunpodEndpoint(base + "/")).toBe("podA");
  });

  it("shellUrlFor includes token query", () => {
    const u = shellUrlFor("https://p-8443.proxy.runpod.net", "sandbox-id", "tok");
    expect(u.startsWith("wss://p-8443.proxy.runpod.net/sandbox/sandbox-id/shell?token=")).toBe(true);
    expect(u).toContain("tok");
  });
});
