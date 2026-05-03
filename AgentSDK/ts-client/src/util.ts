export type RuntimeKind = "runpod_cpu" | "runpod_gpu" | string;

export function runpodProxyBase(podId: string): string {
  const pid = (podId || "").trim();
  if (!pid) return "";
  return `https://${pid}-8443.proxy.runpod.net`;
}

export function deriveDataEndpoint(podProxyUrl?: string | null, podId?: string | null): string {
  const u = (podProxyUrl || "").trim().replace(/\/+$/, "");
  if (u) return u;
  return runpodProxyBase(podId || "").replace(/\/+$/, "");
}

export function stripScheme(hostOrUrl: string): string {
  return hostOrUrl
    .trim()
    .replace(/^https:\/\//i, "")
    .replace(/^http:\/\//i, "")
    .replace(/^wss:\/\//i, "")
    .replace(/^ws:\/\//i, "")
    .replace(/\/+$/, "");
}

export function shellUrlFor(dataEndpoint: string, sandboxId: string, token: string): string {
  const host = stripScheme(dataEndpoint);
  if (!host) return "";
  return `wss://${host}/sandbox/${sandboxId}/shell?token=${encodeURIComponent(token)}`;
}

export function pick(row: Record<string, unknown>, ...keys: string[]): unknown {
  for (const k of keys) {
    if (k in row && row[k] != null) return row[k];
  }
  return undefined;
}

export function parsePodIdFromRunpodEndpoint(url: string): string | undefined {
  const m = /^https?:\/\/(?<pod>[a-zA-Z0-9_-]+)-8443\.proxy\.runpod\.net\/?$/i.exec((url || "").trim().replace(/\/+$/, ""));
  return m?.groups?.pod;
}
