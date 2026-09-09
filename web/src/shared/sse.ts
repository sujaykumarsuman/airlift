import type { FetchFn } from "./api";

export interface SSEEvent {
  event: string;
  data: string;
}

/** Incremental parser for text/event-stream; feed it chunks as they arrive. */
export class SSEParser {
  private buf = "";
  private event = "";
  private data: string[] = [];

  push(chunk: string): SSEEvent[] {
    this.buf += chunk;
    const out: SSEEvent[] = [];
    for (;;) {
      const m = /\r\n|\r|\n/.exec(this.buf);
      if (!m) break;
      if (m[0] === "\r" && m.index === this.buf.length - 1) break; // a \r\n may be split across chunks
      const line = this.buf.slice(0, m.index);
      this.buf = this.buf.slice(m.index + m[0].length);
      const ev = this.line(line);
      if (ev) out.push(ev);
    }
    return out;
  }

  private line(line: string): SSEEvent | null {
    if (line === "") {
      if (this.data.length === 0) {
        this.event = "";
        return null;
      }
      const ev = { event: this.event || "message", data: this.data.join("\n") };
      this.event = "";
      this.data = [];
      return ev;
    }
    if (line.startsWith(":")) return null; // comment / keepalive
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") this.event = value;
    else if (field === "data") this.data.push(value);
    return null;
  }
}

export type SSEStatus = "connecting" | "open" | "retrying" | "stopped";

export interface SubscribeHandlers {
  onEvent(ev: SSEEvent): void;
  onStatus?(status: SSEStatus, detail?: string): void;
}

export interface SubscribeOptions {
  fetchFn?: FetchFn;
  backoffMs?: number[];
  sleep?: (ms: number) => Promise<void>;
}

const defaultBackoff = [500, 1000, 2000, 4000, 8000];

/**
 * Streams SSE through fetch (EventSource cannot send the auth header).
 * Reconnects with backoff; stops for good on 401/404, on an `event: closed`,
 * or when the returned function is called.
 */
export function subscribe(
  url: string,
  token: string,
  handlers: SubscribeHandlers,
  opts: SubscribeOptions = {},
): () => void {
  const fetchFn = opts.fetchFn ?? fetch;
  const backoff = opts.backoffMs ?? defaultBackoff;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const ctrl = new AbortController();
  let stopped = false;
  let attempt = 0;
  const status = (s: SSEStatus, detail?: string) => handlers.onStatus?.(s, detail);

  void (async () => {
    while (!stopped) {
      status("connecting");
      try {
        const resp = await fetchFn(url, {
          headers: { Authorization: `Bearer ${token}`, Accept: "text/event-stream" },
          cache: "no-store",
          signal: ctrl.signal,
        });
        if (resp.status === 401 || resp.status === 404) {
          stopped = true;
          status("stopped", `HTTP ${resp.status}`);
          return;
        }
        if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);
        status("open");
        attempt = 0;
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        const parser = new SSEParser();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const ev of parser.push(decoder.decode(value, { stream: true }))) {
            handlers.onEvent(ev);
            if (ev.event === "closed") {
              stopped = true;
              ctrl.abort();
              status("stopped", "closed");
              return;
            }
          }
        }
        if (!stopped) status("retrying", "stream ended");
      } catch (err) {
        if (stopped) return;
        status("retrying", err instanceof Error ? err.message : String(err));
      }
      if (stopped) return;
      await sleep(backoff[Math.min(attempt, backoff.length - 1)] ?? 1000);
      attempt++;
    }
  })();

  return () => {
    if (stopped) return;
    stopped = true;
    ctrl.abort();
    status("stopped", "unsubscribed");
  };
}
