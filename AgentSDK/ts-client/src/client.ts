import {
  deriveDataEndpoint,
  pick,
  shellUrlFor,
  type RuntimeKind,
} from "./util.js";

import {
  APIError,
  AuthenticationError,
  HostUnreachableError,
  NoCapacityError,
  SandboxNotRunningError,
  TopUpFailedError,
} from "./errors.js";

import { ShellSession } from "./shell.js";

export interface ExecResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  timedOut?: boolean;
  durationMs?: number;
}

export interface FileEntry {
  name: string;
  size: number;
  modUnix: number;
  isDir: boolean;
}

export interface ComputeClientOptions {
  apiUrl: string;
  apiKey?: string;
  supabaseJwt?: string;
  /** Sandbox-scoped bearer (`sbx_…`) returned by `sandbox_create` over MCP. */
  sandboxToken?: string;
  timeoutMs?: number;
  dataJwtRefreshThresholdSecs?: number;
}

function utcNowMs(): number {
  return Date.now();
}

function parseCpError(resp: Response, text: string): { slug: string; msg: string } {
  try {
    const data = JSON.parse(text) as Record<string, unknown>;
    const slug = String(data.code ?? data.error ?? "");
    const msg = String(data.message ?? slug ?? text);
    return { slug, msg };
  } catch {
    return { slug: "", msg: text || resp.statusText };
  }
}

function mapSlug(slug: string, msg: string, status: number): Error | undefined {
  const s = slug.toLowerCase();
  if (s === "no_capacity" || s === "nocapacity" || msg.toLowerCase().includes("no capacity")) {
    return new NoCapacityError(msg, { slug: s || undefined, statusCode: status });
  }
  if (["sandbox_not_running", "paused", "terminated"].includes(s)) {
    return new SandboxNotRunningError(msg, { slug: s || undefined, statusCode: status });
  }
  if (["unauthorized", "forbidden"].includes(s)) {
    return new AuthenticationError(msg, { slug: s || undefined, statusCode: status });
  }
  if (
    s.includes("topup") ||
    s.includes("top_up") ||
    s.includes("deposit") ||
    s === "deposit_required" ||
    s === "out_of_balance" ||
    s === "deposit_mint_failed"
  ) {
    return new TopUpFailedError(msg, { slug: s || undefined, statusCode: status });
  }
  return undefined;
}

async function readError(resp: Response): Promise<never> {
  const text = await resp.text();
  const { slug, msg } = parseCpError(resp, text);
  const mapped = mapSlug(slug, msg, resp.status);
  if (mapped) throw mapped;
  throw new APIError(msg, { slug: slug || undefined, statusCode: resp.status });
}

export class ComputeClient {
  private readonly base: string;
  private readonly timeoutMs: number;
  dataJwtRefreshThresholdSecs: number;
  private readonly opts: ComputeClientOptions;

  constructor(opts: ComputeClientOptions) {
    this.opts = opts;
    this.base = opts.apiUrl.trim().replace(/\/+$/, "");
    this.timeoutMs = opts.timeoutMs ?? 120_000;
    this.dataJwtRefreshThresholdSecs =
      Number(process.env.SOCKT_DATA_JWT_REFRESH_THRESHOLD_SECS) ||
      opts.dataJwtRefreshThresholdSecs ||
      600;
  }

  private cpHeaders(): HeadersInit {
    const key = this.opts.apiKey ?? process.env.SOCKT_API_KEY;
    const jwt =
      this.opts.supabaseJwt ??
      process.env.SOCKT_SUPABASE_JWT ??
      process.env.SOCKT_USER_JWT;
    const sbx = this.opts.sandboxToken ?? process.env.SOCKT_SANDBOX_TOKEN;

    const h = new Headers();
    h.set("Accept", "application/json");
    h.set("Content-Type", "application/json");

    if (key) {
      h.set("Authorization", `Bearer ${key.trim()}`);
    } else if (sbx) {
      h.set("Authorization", `Bearer ${sbx.trim()}`);
    } else if (jwt) {
      h.set("Authorization", `Bearer ${jwt.trim()}`);
    } else {
      throw new AuthenticationError(
        "Set apiKey, supabaseJwt, or sandboxToken (or SOCKT_API_KEY / SOCKT_USER_JWT / SOCKT_SANDBOX_TOKEN).",
      );
    }
    return h;
  }

  async controlPlaneFetch(method: string, path: string, init?: RequestInit): Promise<Response> {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), this.timeoutMs);
    try {
      const { headers: initHeaders, ...restInit } = init ?? {};
      const headers = new Headers(this.cpHeaders());
      if (initHeaders) {
        const extra = new Headers(initHeaders);
        extra.forEach((value, key) => headers.set(key, value));
      }
      return await fetch(`${this.base}${path}`, {
        ...restInit,
        method,
        headers,
        signal: ctrl.signal,
      });
    } catch (e) {
      throw new HostUnreachableError(String(e), { slug: "control_plane_unreachable" });
    } finally {
      clearTimeout(t);
    }
  }

  async createSandbox(body: { billingMethod: string; tier: string }): Promise<Sandbox> {
    const payload: Record<string, unknown> = {
      billing_method: body.billingMethod,
      tier: body.tier,
    };
    const resp = await this.controlPlaneFetch("POST", "/v1/sandbox/create", {
      body: JSON.stringify(payload),
    });
    if (!resp.ok) await readError(resp);
    const row = (await resp.json()) as Record<string, unknown>;
    const sb = sandboxFromRow(this, row);
    await sb.refreshDataJwt(true);
    return sb;
  }

  async getSandbox(id: string): Promise<Sandbox> {
    const resp = await this.controlPlaneFetch("GET", `/v1/sandbox/${id}`);
    if (!resp.ok) await readError(resp);
    const row = (await resp.json()) as Record<string, unknown>;
    const sb = sandboxFromRow(this, row);
    await sb.refreshDataJwt(true);
    return sb;
  }

  listSandboxes(): Promise<Sandbox[]> {
    return Promise.resolve([]);
  }

  async getPricing(): Promise<unknown[]> {
    const resp = await this.controlPlaneFetch("GET", "/v1/pricing");
    if (!resp.ok) await readError(resp);
    const data = await resp.json();
    return Array.isArray(data) ? data : [];
  }

  /** @internal */
  async fetchSandboxRow(id: string): Promise<Record<string, unknown>> {
    const resp = await this.controlPlaneFetch("GET", `/v1/sandbox/${id}`);
    if (!resp.ok) await readError(resp);
    return (await resp.json()) as Record<string, unknown>;
  }

  /** @internal */
  async postRefresh(id: string): Promise<{ token: string; expiresInSecs: number }> {
    const resp = await this.controlPlaneFetch("POST", `/v1/sandbox/${id}/refresh-data-jwt`, {
      body: "{}",
    });
    if (!resp.ok) await readError(resp);
    const data = (await resp.json()) as Record<string, unknown>;
    const tok = pick(data, "data_jwt", "token");
    if (typeof tok !== "string" || !tok.trim()) throw new APIError("refresh missing data_jwt");
    const secsRaw = pick(data, "expires_in_secs", "expires_in");
    let expSecs = 3600;
    if (secsRaw != null) expSecs = Number(secsRaw) || 3600;
    return { token: tok.trim(), expiresInSecs: Math.max(1, expSecs) };
  }

  /** @internal */
  applyRefreshTo(sb: Sandbox, token: string, expiresInSecs: number): void {
    sb.dataJwt = token;
    sb.dataJwtExpiresAt = new Date(utcNowMs() + expiresInSecs * 1000);
  }

  /** @internal */
  async syncSandboxRow(sb: Sandbox): Promise<void> {
    const row = await this.fetchSandboxRow(sb.id);
    sb.raw = row;
    sb.status = String(pick(row, "Status", "status") ?? sb.status);
    sb.tier = String(pick(row, "Tier", "tier") ?? sb.tier);
    sb.template = String(pick(row, "Template", "template") ?? sb.template);
    sb.runtime = String(pick(row, "Runtime", "runtime") ?? sb.runtime) as RuntimeKind;
    sb.billingMethod = String(pick(row, "BillingMethod", "billing_method") ?? sb.billingMethod);
    sb.consumedMsats = Number(pick(row, "ConsumedMsats", "consumed_msats") ?? sb.consumedMsats);
    const podId = pick(row, "PodID", "pod_id");
    sb.podId = podId != null ? String(podId).trim() || null : sb.podId;
    const proxy = pick(row, "PodProxyURL", "pod_proxy_url");
    sb.dataEndpoint = deriveDataEndpoint(
      proxy != null ? String(proxy).trim() : undefined,
      sb.podId,
    ).replace(/\/+$/, "");
  }

  /** @internal */
  async ensureDataJwt(sb: Sandbox, force = false): Promise<void> {
    let need = force;
    if (sb.dataJwtExpiresAt) {
      const remain = (sb.dataJwtExpiresAt.getTime() - utcNowMs()) / 1000;
      if (remain < this.dataJwtRefreshThresholdSecs) need = true;
    } else if (!sb.dataJwt) need = true;
    if (need) await sb.refreshDataJwt(true);
  }

  /** @internal */
  async dataRequest(sb: Sandbox, method: string, path: string, init?: RequestInit): Promise<Response> {
    await this.ensureDataJwt(sb);
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), this.timeoutMs);

    const once = async (): Promise<Response> => {
      const headers = new Headers(init?.headers);
      headers.set("Authorization", `Bearer ${sb.dataJwt}`);
      try {
        return await fetch(`${sb.dataEndpoint.replace(/\/+$/, "")}${path}`, {
          ...init,
          method,
          headers,
          signal: ctrl.signal,
        });
      } catch (e) {
        throw new HostUnreachableError(String(e), { slug: "host_unreachable" });
      }
    };

    try {
      let resp = await once();
      if (resp.status === 401) {
        const ttxt = await resp.clone().text();
        if (ttxt.toLowerCase().includes("data_jwt_expired")) {
          await sb.refreshDataJwt(true);
          resp = await once();
        }
      }
      let bodyText = "";
      if (resp.status === 503 || resp.status === 502) {
        bodyText = await resp.clone().text();
      }
      const looksStarting =
        resp.status === 503 ||
        bodyText.toLowerCase().includes("pod_starting") ||
        bodyText.toLowerCase().includes("starting");
      if (looksStarting) {
        const backoff = Number(process.env.SOCKT_POD_STARTING_BACKOFF_SECS ?? "2") * 1000;
        await new Promise((r) => setTimeout(r, backoff));
        try {
          await sb.refreshDataJwt(true);
        } catch {
          /* ignore */
        }
        resp = await once();
      }
      if (resp.status === 401) {
        throw new AuthenticationError(await resp.text(), { statusCode: 401 });
      }
      if ([503, 502, 504].includes(resp.status)) {
        try {
          await sb.refreshDataJwt(true);
          const r2 = await once();
          if (r2.status < 500) return r2;
        } catch {
          /* fallthrough */
        }
        throw new HostUnreachableError(await resp.text(), {
          slug: "host_unreachable",
          statusCode: resp.status,
        });
      }
      if (!resp.ok) {
        throw new APIError(await resp.text(), { statusCode: resp.status });
      }
      return resp;
    } finally {
      clearTimeout(t);
    }
  }
}

export interface SandboxInit {
  id: string;
  status: string;
  tier: string;
  template: string;
  runtime: RuntimeKind;
  billingMethod: string;
  podId: string | null;
  dataEndpoint: string;
  dataJwt: string;
  dataJwtExpiresAt: Date | null;
  consumedMsats: number;
  sandboxToken: string | null;
  raw: Record<string, unknown> | null;
}

export type { RuntimeKind };

export class Sandbox {
  id: string;
  status: string;
  tier: string;
  template: string;
  runtime: RuntimeKind;
  billingMethod: string;
  podId: string | null;
  dataEndpoint: string;
  dataJwt: string;
  dataJwtExpiresAt: Date | null;
  consumedMsats: number;
  sandboxToken: string | null;
  raw: Record<string, unknown> | null;
  private readonly client: ComputeClient;

  constructor(client: ComputeClient, init: SandboxInit) {
    this.client = client;
    this.id = init.id;
    this.status = init.status;
    this.tier = init.tier;
    this.template = init.template;
    this.runtime = init.runtime;
    this.billingMethod = init.billingMethod;
    this.podId = init.podId;
    this.dataEndpoint = init.dataEndpoint;
    this.dataJwt = init.dataJwt;
    this.dataJwtExpiresAt = init.dataJwtExpiresAt;
    this.consumedMsats = init.consumedMsats;
    this.sandboxToken = init.sandboxToken;
    this.raw = init.raw;
  }

  get shellUrl(): string {
    return shellUrlFor(this.dataEndpoint, this.id, this.dataJwt);
  }

  async ensureShellReady(): Promise<void> {
    await this.client.ensureDataJwt(this);
  }

  async refreshDataJwt(force = false): Promise<void> {
    if (
      !force &&
      this.dataJwtExpiresAt &&
      (this.dataJwtExpiresAt.getTime() - utcNowMs()) / 1000 >= this.client.dataJwtRefreshThresholdSecs
    ) {
      return;
    }
    const { token, expiresInSecs } = await this.client.postRefresh(this.id);
    this.client.applyRefreshTo(this, token, expiresInSecs);
    await this.client.syncSandboxRow(this);
  }

  async exec(command: string, opts?: { timeoutMs?: number; workingDir?: string }): Promise<ExecResult> {
    this.requireRunning();
    const timeoutMs = opts?.timeoutMs ?? 30_000;
    const sec = Math.max(1, Math.min(300, Math.floor(timeoutMs / 1000)));
    const body = {
      argv: ["/bin/bash", "-lc", command],
      cwd: opts?.workingDir ?? ".",
      timeout_seconds: sec,
    };
    const r = await this.client.dataRequest(this, "POST", `/sandbox/${this.id}/exec`, {
      body: JSON.stringify(body),
    });
    const data = (await r.json()) as Record<string, unknown>;
    return {
      stdout: String(data.stdout ?? ""),
      stderr: String(data.stderr ?? ""),
      exitCode: Number(data.exit_code ?? -1),
      timedOut: Boolean(data.timed_out),
      durationMs: Number(data.duration_ms ?? 0),
    };
  }

  async writeFile(
    path: string,
    content: string,
    opts?: { encoding?: "utf8" | "base64"; createDirs?: boolean },
  ): Promise<Record<string, unknown>> {
    this.requireRunning();
    const enc = opts?.encoding ?? "utf8";
    const b64 =
      enc === "base64"
        ? content.trim()
        : Buffer.from(content, "utf8").toString("base64");
    const body = {
      path,
      mode: 0o644,
      content_base64: b64,
      create_dirs: opts?.createDirs ?? true,
    };
    const r = await this.client.dataRequest(this, "POST", `/sandbox/${this.id}/files`, {
      body: JSON.stringify(body),
    });
    return (await r.json()) as Record<string, unknown>;
  }

  async readFile(
    path: string,
    opts?: { encoding?: "utf8" | "base64"; maxBytes?: number },
  ): Promise<{ content: string; meta: Record<string, unknown> }> {
    this.requireRunning();
    const maxBytes = opts?.maxBytes ?? 10_485_760;
    const q = new URLSearchParams({ path });
    const r = await this.client.dataRequest(this, "GET", `/sandbox/${this.id}/files?${q}`);
    const data = (await r.json()) as Record<string, unknown>;
    const rawB64 = String(data.content_base64 ?? "");
    let blob = Buffer.from(rawB64, "base64");
    let truncated = false;
    if (maxBytes > 0 && blob.length > maxBytes) {
      blob = blob.subarray(0, maxBytes);
      truncated = true;
    }
    const meta: Record<string, unknown> = { truncated, path: data.path };
    if ((opts?.encoding ?? "utf8") === "base64") {
      return { content: blob.toString("base64"), meta };
    }
    return { content: blob.toString("utf8"), meta };
  }

  async listFiles(dirPath = "."): Promise<FileEntry[]> {
    this.requireRunning();
    const q = new URLSearchParams({ dir: dirPath });
    const r = await this.client.dataRequest(this, "GET", `/sandbox/${this.id}/files/list?${q}`);
    const data = (await r.json()) as { files?: unknown[] };
    const out: FileEntry[] = [];
    if (Array.isArray(data.files)) {
      for (const fe of data.files) {
        if (!fe || typeof fe !== "object") continue;
        const o = fe as Record<string, unknown>;
        out.push({
          name: String(o.name ?? ""),
          size: Number(o.size ?? 0),
          modUnix: Number(o.mod_unix ?? 0),
          isDir: Boolean(o.is_dir),
        });
      }
    }
    return out;
  }

  async runtimeStatus(): Promise<Record<string, unknown>> {
    this.requireRunning();
    const r = await this.client.dataRequest(this, "GET", `/sandbox/${this.id}/status`);
    return (await r.json()) as Record<string, unknown>;
  }

  async pause(): Promise<void> {
    const resp = await this.client.controlPlaneFetch("POST", `/v1/sandbox/${this.id}/pause`);
    if (!resp.ok) await readError(resp);
    this.status = "paused";
  }

  async resume(): Promise<void> {
    const resp = await this.client.controlPlaneFetch("POST", `/v1/sandbox/${this.id}/resume`);
    if (!resp.ok) await readError(resp);
    this.status = "running";
    await this.refreshDataJwt(true);
  }

  async terminate(): Promise<void> {
    const resp = await this.client.controlPlaneFetch("DELETE", `/v1/sandbox/${this.id}`);
    if (!resp.ok) await readError(resp);
  }

  async extendBalance(opts?: { cents?: number }): Promise<Record<string, unknown>> {
    const body: Record<string, number> = {};
    if (opts?.cents != null) body.cents = opts.cents;
    const resp = await this.client.controlPlaneFetch("POST", `/v1/sandbox/${this.id}/extend-balance`, {
      body: JSON.stringify(body),
    });
    if (!resp.ok) await readError(resp);
    return (await resp.json()) as Record<string, unknown>;
  }

  async waitUntilRunning(opts?: { timeoutSecs?: number; pollIntervalSecs?: number }): Promise<void> {
    const timeoutSecs = opts?.timeoutSecs ?? 120;
    const poll = (opts?.pollIntervalSecs ?? 2) * 1000;
    const deadline = Date.now() + timeoutSecs * 1000;
    while (Date.now() < deadline) {
      const row = await this.client.fetchSandboxRow(this.id);
      const st = String(pick(row, "Status", "status") ?? "").toLowerCase();
      this.status = st;
      if (st === "running") return;
      await new Promise((r) => setTimeout(r, poll));
    }
    throw new SandboxNotRunningError(`sandbox ${this.id} not running within ${timeoutSecs}s`);
  }

  shell(): ShellSession {
    return new ShellSession(this);
  }

  private requireRunning(): void {
    const st = this.status.toLowerCase();
    if (["paused", "terminated", "stopped"].includes(st)) {
      throw new SandboxNotRunningError(`sandbox status is ${this.status}`);
    }
  }
}

function sandboxFromRow(client: ComputeClient, row: Record<string, unknown>): Sandbox {
  const sid = pick(row, "ID", "id");
  if (sid == null) throw new APIError("sandbox id missing");
  const podIdRaw = pick(row, "PodID", "pod_id");
  const podId = podIdRaw != null ? String(podIdRaw).trim() || null : null;
  const proxy = pick(row, "PodProxyURL", "pod_proxy_url");
  const endpoint = deriveDataEndpoint(
    proxy != null ? String(proxy).trim() : undefined,
    podId,
  ).replace(/\/+$/, "");

  const sbxRaw = pick(row, "SandboxToken", "sandbox_token");
  const sandboxToken =
    sbxRaw != null && String(sbxRaw).trim() ? String(sbxRaw).trim() : null;
  return new Sandbox(client, {
    id: String(sid),
    status: String(pick(row, "Status", "status") ?? ""),
    tier: String(pick(row, "Tier", "tier") ?? ""),
    template: String(pick(row, "Template", "template") ?? ""),
    runtime: String(pick(row, "Runtime", "runtime") ?? "") as RuntimeKind,
    billingMethod: String(pick(row, "BillingMethod", "billing_method") ?? ""),
    podId,
    dataEndpoint: endpoint,
    dataJwt: "",
    dataJwtExpiresAt: null,
    consumedMsats: Number(pick(row, "ConsumedMsats", "consumed_msats") ?? 0),
    sandboxToken,
    raw: { ...row },
  });
}
