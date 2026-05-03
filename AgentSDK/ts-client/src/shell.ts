import WebSocket from "ws";

import { shellUrlFor } from "./util.js";

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

  close(): void {
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
  }
}
