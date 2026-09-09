import { expect, test, vi } from "vitest";
import { SSEParser, subscribe, type SSEEvent } from "./sse";

test("parser handles chunk boundaries, comments and CRLF", () => {
  const p = new SSEParser();
  expect(p.push("event: state\nda")).toEqual([]);
  expect(p.push('ta: {"a":1}\n\n: keepalive\n\nevent: closed\r')).toEqual([{ event: "state", data: '{"a":1}' }]);
  expect(p.push("\ndata: {}\r\n\r\n")).toEqual([{ event: "closed", data: "{}" }]);
  expect(p.push("data: one\ndata: two\n\n")).toEqual([{ event: "message", data: "one\ntwo" }]);
  expect(p.push("event: only-name\n\n")).toEqual([]); // no data: nothing dispatched
  expect(p.push("data:nospace\n\n")).toEqual([{ event: "message", data: "nospace" }]);
});

function streamOf(chunks: string[], status = 200): Response {
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(ctrl) {
      for (const c of chunks) ctrl.enqueue(enc.encode(c));
      ctrl.close();
    },
  });
  return new Response(body, { status, headers: { "Content-Type": "text/event-stream" } });
}

test("subscribe streams events, sends the token, and reconnects after the stream ends", async () => {
  const calls: RequestInit[] = [];
  const fetchFn = vi.fn(async (_url: string | URL | Request, init?: RequestInit) => {
    calls.push(init ?? {});
    return calls.length === 1
      ? streamOf(["event: state\ndata: {\"have\":1}\n\n", "event: state\ndata: {\"have\":2}\n\n"])
      : streamOf(["event: closed\ndata: {}\n\n"]);
  });
  const events: SSEEvent[] = [];
  const statuses: string[] = [];
  const done = new Promise<void>((resolve) => {
    subscribe(
      "/api/sessions/x/events",
      "tok",
      {
        onEvent: (e) => events.push(e),
        onStatus: (s, d) => {
          statuses.push(d ? `${s}:${d}` : s);
          if (s === "stopped") resolve();
        },
      },
      { fetchFn, sleep: async () => {} },
    );
  });
  await done;
  expect(events.map((e) => e.data)).toEqual(['{"have":1}', '{"have":2}', "{}"]);
  expect((calls[0]?.headers as Record<string, string>)["Authorization"]).toBe("Bearer tok");
  expect(statuses).toEqual(["connecting", "open", "retrying:stream ended", "connecting", "open", "stopped:closed"]);
});

test("subscribe stops for good on 404 and can be cancelled", async () => {
  const fetchFn = vi.fn(async () => new Response("{\"error\":\"no such session\"}", { status: 404 }));
  const statuses: string[] = [];
  await new Promise<void>((resolve) => {
    subscribe("/x", "t", { onEvent: () => {}, onStatus: (s, d) => { statuses.push(`${s}:${d ?? ""}`); if (s === "stopped") resolve(); } }, { fetchFn });
  });
  expect(statuses).toEqual(["connecting:", "stopped:HTTP 404"]);
  expect(fetchFn).toHaveBeenCalledTimes(1);

  const never = vi.fn((_url: string | URL | Request, init?: RequestInit) => new Promise<Response>((_, reject) => {
    init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
  }));
  const seen: string[] = [];
  const stop = subscribe("/y", "t", { onEvent: () => {}, onStatus: (s) => seen.push(s) }, { fetchFn: never });
  await Promise.resolve();
  stop();
  await Promise.resolve();
  expect(seen).toEqual(["connecting", "stopped"]);
});
