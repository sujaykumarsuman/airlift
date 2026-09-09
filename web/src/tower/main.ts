import "../shared/style.css";
import { ApiError, createSession, deleteSession, eventsURL, fetchDownload, registerClient } from "../shared/api";
import { decodeBitmap, drawBitmap } from "../shared/bitmap";
import { $, html, raw, type Raw } from "../shared/dom";
import { formatBytes, formatDuration } from "../shared/format";
import { subscribe, type SSEStatus } from "../shared/sse";
import type { Beam, ClientSummary, CreateOptions, Snapshot, State, Verdict } from "../shared/types";
import { renderQR } from "./qr";
import { type BeamView, failedStage, initialView, parseDeepLink, reduce, tick, type View } from "./state";

interface Stored {
  sid: string;
  token: string;
  join_url: string;
  client_id: string;
  name: string;
}

const STORAGE_KEY = "airlift.session";
const STATE_LABELS: Record<State, string> = {
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
    current = { sid: deep.sid, token: deep.token, join_url: new URL(`s/${deep.sid}#t=${deep.token}`, appBase).toString(), client_id: "", name: "" };
    history.replaceState(null, "", location.pathname);
  } else {
    const saved = sessionStorage.getItem(STORAGE_KEY);
    if (saved) current = JSON.parse(saved) as Stored;
  }
  if (current) {
    try {
      // Registering (idempotent per address) both validates the session and
      // gives this dashboard its viewer client id.
      const cl = await registerClient(current.sid, current.token, { role: "viewer" });
      current.client_id = cl.client_id;
      current.name = cl.name;
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify(current));
    } catch (err) {
      notice = err instanceof ApiError && err.status === 404 ? "The previous session has expired." : "";
      current = null;
      sessionStorage.removeItem(STORAGE_KEY);
    }
  }
  if (current) attach(current);
  else await renderSession();
}

async function create(opts: CreateOptions = {}): Promise<void> {
  notice = "";
  try {
    const c = await createSession(opts);
    current = { sid: c.sid, token: c.token, join_url: c.join_url, client_id: c.client_id, name: c.name };
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
  stopEvents = subscribe(
    eventsURL(s.sid, "viewer"),
    s.token,
    {
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
    },
    { clientId: s.client_id },
  );
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
      await deleteSession(current.sid, current.token, current.client_id);
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

async function download(bid: string, as: string): Promise<void> {
  if (!current || !bid) return;
  try {
    const { blob, filename } = await fetchDownload(current.sid, current.token, bid, as, current.client_id);
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
      <form id="create-form" class="create-options">
        <label>Label <input id="opt-label" type="text" placeholder="optional" /></label>
        <label>Join password <input id="opt-password" type="password" placeholder="none — token only" /></label>
        <label class="check"><input id="opt-admin" type="checkbox" /> Joiners are session admins</label>
        <p><button class="btn primary" type="submit">Create session</button></p>
      </form>
    </div>`.html;
    $<HTMLFormElement>("#create-form", sessionEl).addEventListener("submit", (e) => {
      e.preventDefault();
      void create({
        label: $<HTMLInputElement>("#opt-label", sessionEl).value.trim() || undefined,
        password: $<HTMLInputElement>("#opt-password", sessionEl).value || undefined,
        joiners_admin: $<HTMLInputElement>("#opt-admin", sessionEl).checked || undefined,
      });
    });
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
  const relays = `${s.relays} ${s.relays === 1 ? "relay" : "relays"}`;
  statusEl.innerHTML = html`
    <div class="card place">
      <div class="head">
        <strong>${s.beams.length} ${s.beams.length === 1 ? "beam" : "beams"}</strong>
        <span class="muted">session ${s.sid} · ${relays} · link ${connection}</span>
      </div>
      ${s.beams.length === 0 ? html`<p class="muted">Waiting for the first beam. Scan a beam page with the phone.</p>` : ""}
      ${s.clients.length ? html`<ul class="clients">${s.clients.map((cl) => clientRow(cl))}</ul>` : ""}
    </div>
    ${view.beams.map((bv) => beamCard(bv))}
    ${notice ? html`<p class="warn">${notice}</p>` : ""}
  `.html;
  for (const bv of view.beams) {
    const b = bv.beam;
    if (b.total > 0) {
      drawBitmap($<HTMLCanvasElement>(`#grid-${b.bid}`, statusEl), decodeBitmap(b.bitmap, b.total), { cell: 10, gap: 2 });
    }
  }
  statusEl.querySelectorAll<HTMLButtonElement>("[data-download]").forEach((btn) =>
    btn.addEventListener("click", () => void download(btn.dataset.beam ?? "", btn.dataset.download ?? "raw")),
  );
}

/** One client in the place's people list. */
function clientRow(cl: ClientSummary): Raw {
  const tags: string[] = [];
  if (cl.session_admin) tags.push("admin");
  tags.push(...cl.roles);
  if (current && cl.client_id === current.client_id) tags.push("you");
  return html`<li class="${cl.connected ? "on" : "off"}">
    <span class="who">${cl.name}</span>${tags.length ? html` <span class="tags">${tags.join(" · ")}</span>` : ""}
  </li>`;
}

/** One beam's card: progress, then a verified or failed panel once terminal. */
function beamCard(bv: BeamView): Raw {
  const b = bv.beam;
  return html`<div class="card beam" data-bid="${b.bid}">
    <div class="head">
      <span class="badge" data-state="${b.state}">${STATE_LABELS[b.state]}</span>
      <strong>${b.name || "(unnamed)"}</strong>
      <span class="muted">beam ${b.bid}</span>
    </div>
    <div class="progress">
      <div class="big">${b.total > 0 ? `${b.have} / ${b.total}` : "— / —"}</div>
      <div class="bar"><div class="fill" style="width: ${bv.pct.toFixed(1)}%"></div></div>
      <div class="metrics">
        <span><b>${b.fps.toFixed(1)}</b> fps decoded</span>
        <span>elapsed <b>${formatDuration(bv.elapsedMs)}</b></span>
        <span>ETA <b>${bv.etaSec === null ? "—" : formatDuration(bv.etaSec * 1000)}</b></span>
      </div>
    </div>
    <canvas id="grid-${b.bid}" class="grid" ${b.total > 0 ? "" : raw("hidden")}></canvas>
    ${b.state === "READY" ? resultCard(b) : ""}
    ${b.state === "FAILED" ? failedCard(b) : ""}
  </div>`;
}

function verdictRow(label: string, v: Verdict | null): Raw {
  if (!v) return raw("");
  return html`<tr class="${v.ok ? "ok" : "bad"}">
    <th>${label}</th>
    <td class="mark">${v.ok ? "✓" : "✗"}</td>
    <td><div>expected <code>${v.expected}</code></div><div>actual <code>${v.actual}</code></div></td>
  </tr>`;
}

function resultCard(b: Beam): Raw {
  const v = b.verdicts;
  return html`<div class="result ok">
    <h3>Verified</h3>
    <table class="verdicts">
      ${verdictRow(STAGE_LABELS.gz_sha, v.gz_sha)}${verdictRow(STAGE_LABELS.orig_sha, v.orig_sha)}${verdictRow(STAGE_LABELS.bundle, v.bundle)}
    </table>
    ${
      b.bundle
        ? html`<p>Repobundle of <b>${b.bundle.files}</b> ${b.bundle.files === 1 ? "file" : "files"}, ${formatBytes(b.bundle.total_bytes)}.</p>
            <ul class="paths">
              ${b.bundle.paths.map((p) => html`<li><code>${p}</code></li>`)}
              ${b.bundle.files > b.bundle.paths.length ? html`<li class="muted">… ${b.bundle.files - b.bundle.paths.length} more</li>` : ""}
            </ul>`
        : html`<p>Not a repobundle: the raw file is the result.</p>`
    }
    <p class="downloads">
      ${b.downloads.map(
        (d) => html`<button class="btn primary" data-beam="${b.bid}" data-download="${d}">Download ${DOWNLOAD_LABELS[d] ?? d}</button>`,
      )}
    </p>
    ${b.saved_path ? html`<p>Written to <code>${b.saved_path}</code></p>` : ""}
    ${b.error ? html`<p class="warn">${b.error}</p>` : ""}
  </div>`;
}

function failedCard(b: Beam): Raw {
  const stage = failedStage(b);
  const v = stage ? b.verdicts[stage] : null;
  return html`<div class="result bad">
    <h3>Failed</h3>
    <p>${b.error ?? "Verification failed."}</p>
    ${
      stage && v
        ? html`<p><b>${STAGE_LABELS[stage]}</b> did not match.</p>
            <table class="verdicts"><tr><th>expected</th><td colspan="2"><code>${v.expected}</code></td></tr><tr><th>actual</th><td colspan="2"><code>${v.actual}</code></td></tr></table>`
        : ""
    }
  </div>`;
}

newButton.addEventListener("click", () => void reset());
void boot();
