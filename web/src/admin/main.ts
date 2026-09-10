import "../shared/style.css";
import {
  adminCancel,
  adminEventsURL,
  adminEvict,
  adminReview,
  adminTerminate,
  ApiError,
  fetchAdminDownload,
  getAdminConfig,
} from "../shared/api";
import { $, html, raw, type Raw } from "../shared/dom";
import { cleanupCountdown, expiryCountdown, terminateCountdown } from "../shared/lifecycle";
import { subscribe, type SSEStatus } from "../shared/sse";
import type { AdminRow, ClientSummary } from "../shared/types";
import { downloadableBeams, reduceAdmin, rowLabel, type AdminView } from "./state";

const loginEl = $<HTMLElement>("#login");
const reviewsEl = $<HTMLElement>("#reviews");
const sessionsEl = $<HTMLElement>("#sessions");
const signOutBtn = $<HTMLButtonElement>("#sign-out");

const ADMIN_KEY = "airlift.admin";
function storage(): Storage | null {
  try {
    return sessionStorage;
  } catch {
    return null;
  }
}

let token = "";
let rows: AdminRow[] = [];
let view: AdminView = { rows: [], pending: [] };
let connection: SSEStatus = "connecting";
let notice = "";
let authed = false;
let stopEvents: (() => void) | null = null;
let ticker: ReturnType<typeof setInterval> | null = null;

const msg = (err: unknown): string => (err instanceof Error ? err.message : String(err));

async function boot(): Promise<void> {
  const saved = storage()?.getItem(ADMIN_KEY) ?? "";
  if (saved) {
    token = saved;
    await tryAuth();
  } else {
    renderLogin();
  }
}

// tryAuth probes the config route (the token's first use); a 401/404 sends the
// operator back to the login form with a reason.
async function tryAuth(): Promise<void> {
  try {
    await getAdminConfig(token);
    authed = true;
    try {
      storage()?.setItem(ADMIN_KEY, token);
    } catch {
      /* per-tab convenience only */
    }
    start();
  } catch (err) {
    authed = false;
    try {
      storage()?.removeItem(ADMIN_KEY);
    } catch {
      /* ignore */
    }
    renderLogin(
      err instanceof ApiError && err.status === 404
        ? "This tower has no admin surface (admin_token is not set)."
        : err instanceof ApiError && err.status === 401
          ? "That token was not accepted."
          : `Could not reach the admin surface: ${msg(err)}`,
    );
  }
}

function start(): void {
  signOutBtn.hidden = false;
  loginEl.innerHTML = "";
  stopEvents?.();
  stopEvents = subscribe(adminEventsURL(), token, {
    onEvent: (ev) => {
      if (ev.event !== "sessions") return;
      try {
        rows = (JSON.parse(ev.data) as { sessions: AdminRow[] }).sessions ?? [];
      } catch {
        rows = [];
      }
      view = reduceAdmin(rows);
      render();
    },
    onStatus: (status, detail) => {
      connection = status;
      // The token stopped working (e.g. admin disabled on a restart): re-login.
      if (status === "stopped" && detail?.startsWith("HTTP")) {
        teardown();
        renderLogin("The admin session ended; sign in again.");
        return;
      }
      render();
    },
  });
  if (ticker === null) ticker = setInterval(patchClocks, 1000);
  render();
}

function teardown(): void {
  stopEvents?.();
  stopEvents = null;
  if (ticker !== null) {
    clearInterval(ticker);
    ticker = null;
  }
  authed = false;
  rows = [];
  view = { rows: [], pending: [] };
  notice = "";
}

function signOut(): void {
  teardown();
  token = "";
  try {
    storage()?.removeItem(ADMIN_KEY);
  } catch {
    /* ignore */
  }
  renderLogin();
}

function renderLogin(message = ""): void {
  signOutBtn.hidden = true;
  reviewsEl.innerHTML = "";
  sessionsEl.innerHTML = "";
  loginEl.innerHTML = html`<div class="card">
    <h2>Admin sign in</h2>
    <p class="muted">Enter the tower's admin token to manage every session.</p>
    ${message ? html`<p class="warn">${message}</p>` : ""}
    <form id="login-form" class="create-options">
      <label>Admin token <input id="admin-token" type="password" placeholder="admin_token" autocomplete="off" /></label>
      <p><button class="btn primary" type="submit">Sign in</button></p>
    </form>
  </div>`.html;
  $<HTMLFormElement>("#login-form", loginEl).addEventListener("submit", (e) => {
    e.preventDefault();
    const t = $<HTMLInputElement>("#admin-token", loginEl).value.trim();
    if (t) {
      token = t;
      void tryAuth();
    }
  });
}

function render(): void {
  if (!authed) return;
  renderReviews();
  renderSessions();
}

function renderReviews(): void {
  if (view.pending.length === 0) {
    reviewsEl.innerHTML = "";
    return;
  }
  reviewsEl.innerHTML = html`<div class="card">
    <h2>Pending reviews <span class="badge">${view.pending.length}</span></h2>
    ${view.pending.map((s) => reviewCard(s))}
  </div>`.html;
  for (const s of view.pending) {
    $<HTMLButtonElement>(`#accept-${s.sid}`, reviewsEl).addEventListener("click", () => void decide(s.sid, "accept"));
    $<HTMLButtonElement>(`#reject-${s.sid}`, reviewsEl).addEventListener("click", () => void decide(s.sid, "reject"));
  }
}

function reviewCard(s: AdminRow): Raw {
  return html`<div class="review" data-sid="${s.sid}">
    <p>
      <strong>${rowLabel(s)}</strong> — ${s.extension?.by || "someone"} asks for more
      time${s.extension?.reason ? html`: “${s.extension.reason}”` : ""}
    </p>
    <div class="controls">
      <input id="note-${s.sid}" type="text" placeholder="note (optional)" />
      <button id="accept-${s.sid}" class="btn small primary" type="button">Accept — reopen</button>
      <button id="reject-${s.sid}" class="btn small" type="button">Reject</button>
    </div>
  </div>`;
}

async function decide(sid: string, decision: "accept" | "reject"): Promise<void> {
  const note = $<HTMLInputElement>(`#note-${sid}`, reviewsEl).value.trim();
  await act(() => adminReview(token, sid, decision, note));
}

function renderSessions(): void {
  sessionsEl.innerHTML = html`
    ${notice ? html`<p class="warn">${notice}</p>` : ""}
    <div class="card">
      <div class="head">
        <h2>Sessions <span class="badge">${view.rows.length}</span></h2>
        <span class="muted">link ${connection}</span>
      </div>
      ${view.rows.length === 0
        ? html`<p class="muted">No sessions yet.</p>`
        : html`<div class="admin-rows">${view.rows.map((s) => sessionRow(s))}</div>`}
    </div>`.html;
  wire();
}

function sessionRow(s: AdminRow): Raw {
  return html`<div class="admin-row" data-sid="${s.sid}">
    <div class="admin-row-head">
      <span class="badge" data-state="${s.status}">${s.status.toLowerCase().replace("_", " ")}</span>
      <strong>${rowLabel(s)}</strong>
      <span class="muted">${s.sid} · ${s.beams.length} ${s.beams.length === 1 ? "beam" : "beams"} · ${s.relays} ${s.relays === 1 ? "relay" : "relays"}</span>
      <span class="clock" id="ck-${s.sid}">${clockText(s)}</span>
    </div>
    <div class="controls">${rowControls(s)}</div>
    ${s.clients.length ? html`<ul class="clients">${s.clients.map((c) => adminClientRow(s, c))}</ul>` : ""}
    ${downloadableBeams(s).length
      ? html`<div class="downloads">${downloadableBeams(s).flatMap((b) =>
          b.downloads.map((d) => html`<button class="btn small" data-dl="${s.sid}" data-bid="${b.bid}" data-as="${d}">↓ ${b.name} · ${d}</button>`),
        )}</div>`
      : ""}
  </div>`;
}

function rowControls(s: AdminRow): Raw {
  if (s.status === "OPEN") {
    return html`<button class="btn small" data-warn="${s.sid}">Terminate…</button
      ><button class="btn small" data-now="${s.sid}">Terminate now</button>`;
  }
  if (s.status === "TERMINATING") {
    return html`<button class="btn small primary" data-cancel="${s.sid}">Cancel</button
      ><button class="btn small" data-now="${s.sid}">Terminate now</button>`;
  }
  return raw("");
}

function adminClientRow(s: AdminRow, c: ClientSummary): Raw {
  const addr = s.addresses[c.client_id] ?? "";
  const tags = [addr, ...(c.session_admin ? ["admin"] : []), ...c.roles].filter(Boolean).join(" · ");
  return html`<li class="${c.connected ? "on" : "off"}">
    <span class="who">${c.name}</span>${tags ? html` <span class="tags">${tags}</span>` : ""}
    <button class="btn small" data-evict="${s.sid}" data-cid="${c.client_id}">Evict</button>
  </li>`;
}

function wire(): void {
  const on = (sel: string, run: (b: HTMLElement) => void): void =>
    sessionsEl.querySelectorAll<HTMLElement>(sel).forEach((b) => b.addEventListener("click", () => run(b)));
  on("[data-warn]", (b) => void act(() => adminTerminate(token, b.dataset.warn!, false)));
  on("[data-now]", (b) => void act(() => adminTerminate(token, b.dataset.now!, true)));
  on("[data-cancel]", (b) => void act(() => adminCancel(token, b.dataset.cancel!)));
  on("[data-evict]", (b) => void act(() => adminEvict(token, b.dataset.evict!, b.dataset.cid!)));
  on("[data-dl]", (b) => void downloadBeam(b.dataset.dl!, b.dataset.bid!, b.dataset.as!));
}

// act runs an admin mutation; the SSE reflects the result, so success just clears
// any prior notice while a failure surfaces the message.
async function act(fn: () => Promise<void>): Promise<void> {
  try {
    await fn();
    if (notice) {
      notice = "";
      render();
    }
  } catch (err) {
    notice = `Action failed: ${msg(err)}`;
    render();
  }
}

async function downloadBeam(sid: string, bid: string, as: string): Promise<void> {
  try {
    const { blob, filename } = await fetchAdminDownload(token, sid, bid, as);
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30_000);
  } catch (err) {
    notice = `Download failed: ${msg(err)}`;
    render();
  }
}

function clockText(s: AdminRow): string {
  const now = Date.now();
  if (s.status === "OPEN") {
    const c = expiryCountdown(s, now);
    return c.hidden ? "" : `expires in ${c.text}`;
  }
  if (s.status === "TERMINATING") {
    const c = terminateCountdown(s, now);
    return c.hidden ? "ending" : `ending in ${c.text}`;
  }
  if ((s.status === "TERMINATED" || s.status === "REJECTED") && s.terminated) {
    const c = cleanupCountdown(s.terminated, now);
    return c.done ? "files removed" : `files kept ${c.text}`;
  }
  if (s.status === "PENDING_REVIEW") return "awaiting review";
  return "";
}

// patchClocks refreshes only the per-row countdown text, so the admin SSE (which
// pushes only on a change) does not freeze the countdowns and no control or note
// input is rebuilt on the tick.
function patchClocks(): void {
  if (!authed) return;
  for (const s of view.rows) {
    const el = sessionsEl.querySelector<HTMLElement>(`#ck-${s.sid}`);
    if (el) el.textContent = clockText(s);
  }
}

signOutBtn.addEventListener("click", signOut);
void boot();
