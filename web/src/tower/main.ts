import "../shared/style.css";
import { ApiError, createSession, deleteSession, eventsURL, fetchDownload, getSnapshot } from "../shared/api";
import { decodeBitmap, drawBitmap } from "../shared/bitmap";
import { $, html, raw, type Raw } from "../shared/dom";
import { formatBytes, formatDuration, hex8 } from "../shared/format";
import { subscribe, type SSEStatus } from "../shared/sse";
import type { Snapshot, State, Verdict } from "../shared/types";
import { renderQR } from "./qr";
import { failedStage, initialView, parseDeepLink, reduce, tick, type View } from "./state";

interface Stored {
  sid: string;
  token: string;
  join_url: string;
}

const STORAGE_KEY = "airlift.session";
const STATE_LABELS: Record<State, string> = {
  WAITING_MANIFEST: "waiting for manifest",
  RECEIVING: "receiving",
  VERIFYING: "verifying",
  READY: "ready",
  FAILED: "failed",
};
const DOWNLOAD_LABELS: Record<string, string> = { raw: "raw file", file: "file", zip: "zip of the tree" };
const STAGE_LABELS = { gz_sha: "gzip blob sha256", orig_sha: "original sha256", bundle: "bundle files" } as const;

const sessionEl = $<HTMLElement>("#session");
const statusEl = $<HTMLElement>("#status");
const newButton = $<HTMLButtonElement>("#new-session");

let current: Stored | null = null;
let view: View = initialView;
let connection: SSEStatus = "connecting";
let stopEvents: (() => void) | null = null;
let ticker: ReturnType<typeof setInterval> | null = null;
let notice = "";

// The app root, incl. any path prefix from the injected <base href>.
const appBase = new URL("./", document.baseURI).toString();

function joinLink(s: Stored): string {
  // In `vite dev` the phone must reach the dev server, not the tower.
  return import.meta.env.DEV ? new URL(`s/${s.sid}#t=${s.token}`, appBase).toString() : s.join_url;
}

function viewerLink(s: Stored): string {
  return new URL(`#s=${s.sid}&t=${s.token}`, appBase).toString();
}

async function boot(): Promise<void> {
  const deep = parseDeepLink(location.hash);
  if (deep) {
    current = { ...deep, join_url: new URL(`s/${deep.sid}#t=${deep.token}`, appBase).toString() };
    history.replaceState(null, "", location.pathname);
  } else {
    const saved = sessionStorage.getItem(STORAGE_KEY);
    if (saved) current = JSON.parse(saved) as Stored;
  }
  if (current) {
    try {
      await getSnapshot(current.sid, current.token);
    } catch (err) {
      notice = err instanceof ApiError && err.status === 404 ? "The previous session has expired." : "";
      current = null;
      sessionStorage.removeItem(STORAGE_KEY);
    }
  }
  if (current) attach(current);
  else await renderSession();
}

async function create(): Promise<void> {
  notice = "";
  try {
    const c = await createSession();
    current = { sid: c.sid, token: c.token, join_url: c.join_url };
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(current));
    attach(current);
  } catch (err) {
    notice = err instanceof Error ? err.message : String(err);
    await renderSession();
  }
}

function attach(s: Stored): void {
  view = initialView;
  connection = "connecting";
  stopEvents?.();
  stopEvents = subscribe(eventsURL(s.sid, "viewer"), s.token, {
    onEvent: (ev) => {
      if (ev.event === "state") view = reduce(view, JSON.parse(ev.data) as Snapshot, Date.now());
      else if (ev.event === "closed") notice = "The session was closed.";
      renderStatus();
    },
    onStatus: (status, detail) => {
      connection = status;
      if (status === "stopped" && detail?.startsWith("HTTP")) notice = `Session gone (${detail}).`;
      renderStatus();
    },
  });
  if (ticker === null) {
    ticker = setInterval(() => {
      const next = tick(view, Date.now());
      if (next !== view) {
        view = next;
        renderStatus();
      }
    }, 1000);
  }
  void renderSession();
  renderStatus();
}

async function reset(): Promise<void> {
  if (current) {
    try {
      await deleteSession(current.sid, current.token);
    } catch {
      /* already gone */
    }
  }
  stopEvents?.();
  stopEvents = null;
  current = null;
  sessionStorage.removeItem(STORAGE_KEY);
  await create();
}

async function download(as: string): Promise<void> {
  if (!current) return;
  try {
    const { blob, filename } = await fetchDownload(current.sid, current.token, as);
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30_000);
  } catch (err) {
    notice = `Download failed: ${err instanceof Error ? err.message : String(err)}`;
    renderStatus();
  }
}

async function renderSession(): Promise<void> {
  newButton.hidden = current === null;
  if (!current) {
    sessionEl.innerHTML = html`<div class="card">
      ${notice ? html`<p class="warn">${notice}</p>` : ""}
      <p>No session yet. Create one, then scan its code with the phone.</p>
      <p><button class="btn primary" id="create">Create session</button></p>
    </div>`.html;
    $<HTMLButtonElement>("#create", sessionEl).addEventListener("click", () => void create());
    return;
  }
  const link = joinLink(current);
  sessionEl.innerHTML = html`<div class="card join">
    <canvas id="join-qr" width="256" height="256"></canvas>
    <div class="join-text">
      <h2>Join with the phone</h2>
      <p>Scan this code with the phone's camera app, or open the link:</p>
      <p><code class="url">${link}</code> <button class="btn small" id="copy">Copy</button></p>
      <p class="hint">Watch from another device: <code class="url">${viewerLink(current)}</code></p>
      <p class="muted">session ${current.sid}</p>
    </div>
  </div>`.html;
  $<HTMLButtonElement>("#copy", sessionEl).addEventListener("click", () => void navigator.clipboard?.writeText(link));
  try {
    await renderQR($<HTMLCanvasElement>("#join-qr", sessionEl), link);
  } catch {
    /* the link is still shown as text */
  }
}

function renderStatus(): void {
  const s = view.snap;
  if (!current) {
    statusEl.innerHTML = "";
    return;
  }
  if (!s) {
    statusEl.innerHTML = html`<div class="card"><p class="muted">Connecting to the session… (${connection})</p>${
      notice ? html`<p class="warn">${notice}</p>` : ""
    }</div>`.html;
    return;
  }
  statusEl.innerHTML = html`
    <div class="card">
      <div class="head">
        <span class="badge" data-state="${s.state}">${STATE_LABELS[s.state]}</span>
        <strong>${s.name || "waiting for the manifest"}</strong>
        <span class="muted">sender ${hex8(s.sender_session)} · ${s.relays} ${s.relays === 1 ? "relay" : "relays"} · link ${connection}</span>
      </div>
      <div class="progress">
        <div class="big">${s.total > 0 ? `${s.have} / ${s.total}` : "— / —"}</div>
        <div class="bar"><div class="fill" style="width: ${view.pct.toFixed(1)}%"></div></div>
        <div class="metrics">
          <span><b>${s.fps.toFixed(1)}</b> fps decoded</span>
          <span>elapsed <b>${formatDuration(view.elapsedMs)}</b></span>
          <span>ETA <b>${view.etaSec === null ? "—" : formatDuration(view.etaSec * 1000)}</b></span>
        </div>
      </div>
      <canvas id="grid" class="grid" ${s.total > 0 ? "" : raw("hidden")}></canvas>
    </div>
    ${s.state === "READY" ? resultCard(s) : ""}
    ${s.state === "FAILED" ? failedCard(s) : ""}
    ${notice ? html`<p class="warn">${notice}</p>` : ""}
  `.html;
  if (s.total > 0) drawBitmap($<HTMLCanvasElement>("#grid", statusEl), decodeBitmap(s.bitmap, s.total), { cell: 10, gap: 2 });
  statusEl.querySelectorAll<HTMLButtonElement>("[data-download]").forEach((b) =>
    b.addEventListener("click", () => void download(b.dataset.download ?? "raw")),
  );
  statusEl.querySelector<HTMLButtonElement>("[data-reset]")?.addEventListener("click", () => void reset());
}

function verdictRow(label: string, v: Verdict | null): Raw {
  if (!v) return raw("");
  return html`<tr class="${v.ok ? "ok" : "bad"}">
    <th>${label}</th>
    <td class="mark">${v.ok ? "✓" : "✗"}</td>
    <td><div>expected <code>${v.expected}</code></div><div>actual <code>${v.actual}</code></div></td>
  </tr>`;
}

function resultCard(s: Snapshot): Raw {
  const v = s.verdicts;
  return html`<div class="card ok">
    <h2>Verified</h2>
    <table class="verdicts">
      ${verdictRow(STAGE_LABELS.gz_sha, v.gz_sha)}${verdictRow(STAGE_LABELS.orig_sha, v.orig_sha)}${verdictRow(STAGE_LABELS.bundle, v.bundle)}
    </table>
    ${
      s.bundle
        ? html`<p>Repobundle of <b>${s.bundle.files}</b> ${s.bundle.files === 1 ? "file" : "files"}, ${formatBytes(s.bundle.total_bytes)}.</p>
            <ul class="paths">
              ${s.bundle.paths.map((p) => html`<li><code>${p}</code></li>`)}
              ${s.bundle.files > s.bundle.paths.length ? html`<li class="muted">… ${s.bundle.files - s.bundle.paths.length} more</li>` : ""}
            </ul>`
        : html`<p>Not a repobundle: the raw file is the result.</p>`
    }
    <p class="downloads">
      ${s.downloads.map((d) => html`<button class="btn primary" data-download="${d}">Download ${DOWNLOAD_LABELS[d] ?? d}</button>`)}
    </p>
    ${s.dest_path ? html`<p>Written to <code>${s.dest_path}</code></p>` : ""}
    ${s.error ? html`<p class="warn">${s.error}</p>` : ""}
  </div>`;
}

function failedCard(s: Snapshot): Raw {
  const stage = failedStage(s);
  const v = stage ? s.verdicts[stage] : null;
  return html`<div class="card bad">
    <h2>Failed</h2>
    <p>${s.error ?? "Verification failed."}</p>
    ${
      stage && v
        ? html`<p><b>${STAGE_LABELS[stage]}</b> did not match.</p>
            <table class="verdicts"><tr><th>expected</th><td colspan="2"><code>${v.expected}</code></td></tr><tr><th>actual</th><td colspan="2"><code>${v.actual}</code></td></tr></table>`
        : ""
    }
    <p><button class="btn" data-reset>Reset: start a new session</button></p>
  </div>`;
}

newButton.addEventListener("click", () => void reset());
void boot();
