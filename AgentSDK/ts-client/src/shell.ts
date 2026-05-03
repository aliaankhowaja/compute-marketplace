import WebSocket from "ws";

import { shellUrlFor } from "./util.js";

/** Raw JSON object from the runtime shell WebSocket (stdin/stdout/billing/deposit hints). */
export type ShellServerFrame = Record<string, unknown>;

/** Sandbox subset needed for interactive shell (avoids circular imports). */
export interface ShellSessionSandbox {
  id: string;
  dataEndpoint: string;
  dataJwt: string;
  ensureShellReady(): Promise<void>;
}

export class ShellSession {
  private ws: WebSocket | null = null;

  constructor(private readonly sb: ShellSessionSandbox) {}

  async connect(): Promise<void> {
    await this.sb.ensureShellReady();
    const uri = shellUrlFor(this.sb.dataEndpoint, this.sb.id, this.sb.dataJwt);
    this.ws = new WebSocket(uri, {
      headers: { Authorization: `Bearer ${this.sb.dataJwt}` },
    });
    await new Promise<void>((resolve, reject) => {
      if (!this.ws) return reject(new Error("ws missing"));
      this.ws.once("open", () => resolve());
      this.ws.once("error", reject);
    });
  }

  sendRaw(payload: Record<string, unknown>): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      throw new Error("shell websocket not connected");
    }
    this.ws.send(JSON.stringify(payload));
  }

  send(text: string): void {
    this.sendRaw({ type: "stdin", data: text });
  }

  sendLine(text: string): void {
    this.send(text + "\n");
  }

  resize(cols: number, rows: number): void {
    this.sendRaw({ type: "resize", cols, rows });
  }

  /**
   * Subscribe to every server JSON frame (`billing`, `low_balance`, `deposit_required`, stdout, etc.).
   * Call after `connect()`. Multiple listeners stack.
   */
  onFrame(cb: (frame: ShellServerFrame) => void): void {
    if (!this.ws) {
      throw new Error("shell websocket not connected");
    }
    this.ws.on("message", (buf) => {
      try {
        const data = JSON.parse(String(buf)) as ShellServerFrame;
        cb(data);
      } catch {
        /* ignore malformed */
      }
    });
  }

  /** Await next JSON frame or timeout (rejects on timeout). */
  recv(timeoutMs = 30_000): Promise<ShellServerFrame> {
    const ws = this.ws;
    if (!ws) {
      return Promise.reject(new Error("shell websocket not connected"));
    }
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        cleanup();
        reject(new Error("shell recv timeout"));
      }, timeoutMs);
      const onMessage = (buf: WebSocket.RawData) => {
        try {
          const data = JSON.parse(String(buf)) as ShellServerFrame;
          cleanup();
          resolve(data);
        } catch {
          /* wait for next */
        }
      };
      const onClose = () => {
        cleanup();
        reject(new Error("shell websocket closed"));
      };
      const cleanup = () => {
        clearTimeout(timer);
        ws.off("message", onMessage);
        ws.off("close", onClose);
      };
      ws.on("message", onMessage);
      ws.once("close", onClose);
    });
  }

  close(): void {
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
  }
}
