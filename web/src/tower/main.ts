import "../shared/style.css";
import { ApiError, createSession, deleteBeam, deleteClient, deleteSession, eventsURL, fetchDownload, getInfo, getKnockStatus, joinSession, postExtension, postExtendMaxAge, postKnock, postPing, registerClient, resolveKnock } from "../shared/api";
import { decodeBitmap } from "../shared/bitmap";
import { renderChunkMarks } from "../shared/chunks";
import { bindCopyButtons } from "../shared/copy";
import { $, html, raw, type Raw } from "../shared/dom";
import { icon } from "../shared/icons";
import { formatBytes, formatDuration } from "../shared/format";
import { enter } from "../shared/motion";
import { cleanupCountdown, expiryCountdown, instantMs, terminateCountdown, terminatedBy, terminatedWhy } from "../shared/lifecycle";
import { bindActivity, Pinger, type PingOutcome } from "../shared/ping";
import { subscribe, type SSEStatus } from "../shared/sse";
import type { Beam, ClientSummary, CreateOptions, KnockView, Snapshot, State, Verdict } from "../shared/types";
import { renderQR } from "./qr";
import { type BeamView, failedStage, initialView, reduce, tick, type View } from "./state";

interface Stored {
  sid: string;
  token: string;
  client_id: string;
  name: string;
  hasPassword: boolean; // how this device got in: password join, or a token link
}

const storageKey = (sid: string): string => `airlift.session.${sid}`;
function loadStored(sid: string): Stored | null {
  try {
    const raw = sessionStorage.getItem(storageKey(sid));
    return raw ? (JSON.parse(raw) as Stored) : null;
  } catch {
    return null;
  }
}
function saveStored(s: Stored): void {
  try {
    sessionStorage.setItem(storageKey(s.sid), JSON.stringify(s));
  } catch {
    /* private mode: the URL still carries enough to rejoin a public session */
  }
}
function clearStored(sid: string): void {
  try {
    sessionStorage.removeItem(storageKey(sid));
  } catch {
    /* ignore */
  }
}

const STATE_LABELS: Record<State, string> = {
  RECEIVING: "receiving",
  VERIFYING: "verifying",
  READY: "ready",
  FAILED: "failed",
};
const DOWNLOAD_LABELS: Record<string, string> = { raw: "raw file", file: "file", zip: "zip of the tree" };
const STAGE_LABELS = { gz_sha: "gzip blob sha256", orig_sha: "original sha256", bundle: "bundle files" } as const;

const appEl = $<HTMLElement>("#app");
const sessionEl = $<HTMLElement>("#session");
const placeEl = $<HTMLElement>("#place"); // session panel (left column, below the share card)
const statusEl = $<HTMLElement>("#status"); // beams (right column)
const newButton = $<HTMLButtonElement>("#new-session");
// The landing: a hero above the Create / Join card and the get-airlift / how-it-
// works sections below it (static HTML), shown on the home page only.
const heroEl = $<HTMLElement>("#hero");
const landingEl = $<HTMLElement>("#landing");
// The session-admin actions live in the nav: a power button opening End / Delete.
const menuEl = $<HTMLElement>("#session-menu");
const powerBtn = $<HTMLButtonElement>("#power");
const endBtn = $<HTMLButtonElement>("#end-btn");
const delBtn = $<HTMLButtonElement>("#del-btn");

// The layout is a single centred column for the home/gate screens and a two-column
// dashboard (share + participants | beams) once a session is attached (ADR 0019).
function setMode(mode: "home" | "dash"): void {
  const next = mode === "home" ? "mode-home" : "mode-dash";
  if (!appEl.classList.contains(next)) {
    appEl.className = next;
    enter(appEl); // a new view: its cards rise in
  }
  heroEl.hidden = landingEl.hidden = true; // only renderHome shows the landing
  if (mode === "home") showMenu(false, false);
}

// showVersion fills the landing footer with the tower's version, once.
let versionShown = false;
function showVersion(): void {
  if (versionShown) return;
  versionShown = true;
  void getInfo()
    .then((i) => {
      const el = document.getElementById("tower-version");
      if (el) el.textContent = `tower ${i.version}`;
    })
    .catch(() => {
      /* the footer simply stays blank */
    });
}

// bindCloneTabs wires the landing's SSH / HTTPS / ZIP pill: the thumb slides
// (--i) and the matching command pane shows.
function bindCloneTabs(): void {
  const seg = document.querySelector<HTMLElement>("[data-clone]");
  if (!seg) return;
  const tabs = [...seg.querySelectorAll<HTMLButtonElement>(".seg")];
  const panes = [...landingEl.querySelectorAll<HTMLElement>("[data-pane]")];
  tabs.forEach((b, i) =>
    b.addEventListener("click", () => {
      seg.style.setProperty("--i", String(i));
      for (const t of tabs) {
        const on = t === b;
        t.classList.toggle("on", on);
        t.setAttribute("aria-selected", String(on));
      }
      for (const p of panes) p.hidden = p.dataset.pane !== b.dataset.tab;
    }),
  );
}

// Beams already on the dashboard; a bid not in here is a new card and rises in.
const seenBids = new Set<string>();

// showMenu shows the power menu to a session admin; End only while the session is live.
function showMenu(admin: boolean, live: boolean): void {
  menuEl.hidden = !admin;
  endBtn.hidden = !live;
  if (!admin) closeMenu();
}
function closeMenu(): void {
  menuEl.classList.remove("open");
  powerBtn.setAttribute("aria-expanded", "false");
}

let current: Stored | null = null;
let view: View = initialView;
let connection: SSEStatus = "connecting";
let stopEvents: (() => void) | null = null;
let ticker: ReturnType<typeof setInterval> | null = null;
let notice = "";
let pinger: Pinger | null = null;
let clocksClosed = false; // guards the one-shot re-render when the cleanup countdown ends
let knockTimer: ReturnType<typeof setInterval> | null = null; // polls a pending knock (ADR 0021)
let homeTab: "create" | "join" = "create"; // the home page's Create / Join switch

// The app root, incl. any path prefix from the injected <base href>.
const appBase = new URL("./", document.baseURI).toString();

// The session's own URL (ADR 0020). A public session appends its token for
// one-tap join; a password session shares only the id (the joiner enters the
// password).
function joinLink(sid: string, token: string, hasPassword: boolean): string {
  const url = new URL(sid, appBase).toString();
  return hasPassword || !token ? url : `${url}#t=${token}`;
}

// The scanner for this session, opened on demand by the Scan button (carries the
// token — the dashboard already holds it).
function scanLink(s: Stored): string {
  // `c=` lets the scanner resume this device's client, so one device is one
  // participant with both roles rather than two entries (ADR 0022).
  return new URL(`s/${s.sid}#t=${s.token}${s.client_id ? `&c=${s.client_id}` : ""}`, appBase).toString();
}

// The session id from the current path ("" on the home page).
function sidFromPath(): string {
  const base = new URL(document.baseURI).pathname; // "/airlift/" or "/"
  const rest = location.pathname.startsWith(base) ? location.pathname.slice(base.length) : location.pathname.replace(/^\//, "");
  return rest.replace(/\/+$/, "").trim();
}

// The token from the URL fragment, if any (#t=…).
function tokenFromHash(): string {
  return new URLSearchParams(location.hash.replace(/^#/, "")).get("t")?.trim() ?? "";
}

async function boot(): Promise<void> {
  const sid = sidFromPath();
  if (!sid) {
    await renderHome(); // the home page: create, or join by id
    return;
  }
  // A session URL `…/<sid>`. The token comes from the fragment (a public link) or
  // from what this device stored when it last joined; a password session has
  // neither, so it falls to the gate.
  const frag = tokenFromHash();
  const stored = loadStored(sid);
  const token = frag || stored?.token || "";
  if (frag) history.replaceState(null, "", location.pathname); // drop the token from the URL bar
  current = { sid, token, client_id: stored?.client_id ?? "", name: stored?.name ?? "", hasPassword: stored?.hasPassword ?? false };
  if (token) await enterWithToken();
  else await probeGate(sid);
}

// enterWithToken registers this dashboard as a viewer using the token in hand.
async function enterWithToken(): Promise<void> {
  if (!current) return;
  try {
    const cl = await registerClient(current.sid, current.token, { role: "viewer", resume: current.client_id || undefined });
    current.client_id = cl.client_id;
    current.name = cl.name;
    saveStored(current);
    attach(current);
  } catch (err) {
    if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
      // A stale/wrong token for this id — fall back to the gate (password or link).
      clearStored(current.sid);
      current.token = "";
      await probeGate(current.sid);
    } else {
      notice = err instanceof ApiError && err.status === 404 ? "That session was not found (it may have expired)." : err instanceof Error ? err.message : String(err);
      current = null;
      await renderHome();
    }
  }
}

// probeGate classifies a session opened without a token: an empty-password join is
// always rejected, but 401 means the session is password-protected (show the
// form) while 404 means it is public/needs its link (or does not exist) — the
// privacy split from ADR 0017.
async function probeGate(sid: string): Promise<void> {
  try {
    await joinSession(sid, { password: "" });
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      renderPasswordGate(sid);
      return;
    }
  }
  renderKnockGate(sid); // public (or missing) — ask to be admitted (ADR 0021)
}

async function create(opts: CreateOptions = {}): Promise<void> {
  notice = "";
  try {
    const c = await createSession(opts);
    current = { sid: c.sid, token: c.token, client_id: c.client_id, name: c.name, hasPassword: !!opts.password };
    saveStored(current);
    history.pushState(null, "", new URL(c.sid, appBase).toString()); // move to …/<sid>
    attach(current);
  } catch (err) {
    notice = err instanceof Error ? err.message : String(err);
    await renderHome();
  }
}

// The home page: create a session, or join one by id.
async function renderHome(): Promise<void> {
  current = null;
  newButton.hidden = true;
  setMode("home");
  placeEl.innerHTML = "";
  statusEl.innerHTML = "";
  // Both cards are rendered once; the pill switch slides its thumb and swaps
  // which card is shown, without a re-render, so the motion actually plays.
  sessionEl.innerHTML = html`
    ${notice ? html`<div class="card"><p class="warn">${notice}</p></div>` : ""}
    <div class="segmented" role="tablist" style="--i:${homeTab === "join" ? 1 : 0}">
      <span class="thumb" aria-hidden="true"></span>
      <button class="btn seg${homeTab === "create" ? " on" : ""}" type="button" role="tab" data-tab="create" aria-selected="${homeTab === "create"}">${icon("plus")} Create</button>
      <button class="btn seg${homeTab === "join" ? " on" : ""}" type="button" role="tab" data-tab="join" aria-selected="${homeTab === "join"}">${icon("key")} Join</button>
    </div>
    <div id="tab-create" class="card" ${homeTab === "create" ? "" : raw("hidden")}>
      <p class="section-label">${icon("beam")} Create a session</p>
      <form id="create-form" class="create-options">
        <label>Join password — optional <input id="opt-password" type="password" placeholder="none — open to anyone with the link" /></label>
        <label class="check"><input id="opt-admin" type="checkbox" /> Joiners are session admins</label>
        <p><button class="btn primary" type="submit">${icon("plus")} Create session</button></p>
      </form>
    </div>
    <div id="tab-join" class="card" ${homeTab === "join" ? "" : raw("hidden")}>
      <p class="section-label">${icon("key")} Join a session</p>
      <form id="join-form" class="create-options">
        <label>Session id <input id="join-sid" type="text" placeholder="e.g. qkf-mzt-bwp" autocomplete="off" spellcheck="false" /></label>
        <p><button class="btn primary" type="submit">${icon("key")} Join</button></p>
      </form>
    </div>`.html;
  const switchEl = $<HTMLElement>(".segmented", sessionEl);
  const segs = [...sessionEl.querySelectorAll<HTMLButtonElement>(".seg")];
  const showTab = (tab: "create" | "join"): void => {
    if (tab === homeTab) return;
    homeTab = tab;
    switchEl.style.setProperty("--i", tab === "join" ? "1" : "0");
    for (const b of segs) {
      const on = b.dataset.tab === tab;
      b.classList.toggle("on", on);
      b.setAttribute("aria-selected", String(on));
    }
    const create = $<HTMLElement>("#tab-create", sessionEl);
    const join = $<HTMLElement>("#tab-join", sessionEl);
    create.hidden = tab !== "create";
    join.hidden = tab !== "join";
    enter(tab === "create" ? create : join);
    (tab === "create" ? $<HTMLInputElement>("#opt-password", create) : $<HTMLInputElement>("#join-sid", join)).focus();
  };
  for (const b of segs) b.addEventListener("click", () => showTab(b.dataset.tab === "join" ? "join" : "create"));
  $<HTMLFormElement>("#create-form", sessionEl).addEventListener("submit", (e) => {
    e.preventDefault();
    void create({
      password: $<HTMLInputElement>("#opt-password", sessionEl).value || undefined,
      joiners_admin: $<HTMLInputElement>("#opt-admin", sessionEl).checked || undefined,
    });
  });
  $<HTMLFormElement>("#join-form", sessionEl).addEventListener("submit", (e) => {
    e.preventDefault();
    const sid = $<HTMLInputElement>("#join-sid", sessionEl).value.trim().toLowerCase();
    if (sid) location.href = new URL(sid, appBase).toString();
  });
  heroEl.hidden = false;
  landingEl.hidden = false;
  showVersion();
}

// A password-protected session opened without a token: ask for the password.
function renderPasswordGate(sid: string): void {
  newButton.hidden = false;
  setMode("home");
  placeEl.innerHTML = "";
  statusEl.innerHTML = "";
  sessionEl.innerHTML = html`<div class="card">
    <p class="section-label">${icon("lock")} Password gate</p>
    <h2>Join <code>${sid}</code></h2>
    <p class="muted">This session is password-protected.</p>
    <form id="pw-form" class="create-options">
      <label>Your name <input id="pw-name" type="text" placeholder="optional" /></label>
      <label>Password <input id="pw-pass" type="password" placeholder="session password" /></label>
      <p><button class="btn primary" type="submit">${icon("lock")} Join</button></p>
      ${notice ? html`<p class="warn">${notice}</p>` : ""}
    </form>
  </div>`.html;
  $<HTMLFormElement>("#pw-form", sessionEl).addEventListener("submit", (e) => {
    e.preventDefault();
    void passwordJoin(sid, $<HTMLInputElement>("#pw-pass", sessionEl).value, $<HTMLInputElement>("#pw-name", sessionEl).value.trim());
  });
}

async function passwordJoin(sid: string, password: string, name: string): Promise<void> {
  notice = "";
  try {
    const j = await joinSession(sid, { password, name: name || undefined });
    current = { sid, token: j.token, client_id: j.client_id, name: j.name, hasPassword: true };
    saveStored(current);
    attach(current);
  } catch (err) {
    notice = err instanceof ApiError && err.status === 401 ? "Wrong password." : err instanceof Error ? err.message : String(err);
    renderPasswordGate(sid);
  }
}

// A public session opened without its token: ask the session admin to admit you
// (ADR 0021). No token, no password — the admin is the gate.
function renderKnockGate(sid: string): void {
  newButton.hidden = false;
  setMode("home");
  placeEl.innerHTML = "";
  statusEl.innerHTML = "";
  sessionEl.innerHTML = html`<div class="card">
    <p class="section-label">${icon("bell")} Knock gate</p>
    <h2>Join <code>${sid}</code></h2>
    <p class="muted">This session is invite-only from a bare id. Ask the session admin to let you in.</p>
    <form id="knock-form" class="create-options">
      <label>Your name <input id="knock-name" type="text" placeholder="so the admin knows who you are" /></label>
      <p><button class="btn primary" type="submit">${icon("bell")} Ask to join</button></p>
      ${notice ? html`<p class="warn">${notice}</p>` : ""}
    </form>
  </div>`.html;
  $<HTMLFormElement>("#knock-form", sessionEl).addEventListener("submit", (e) => {
    e.preventDefault();
    void knock(sid, $<HTMLInputElement>("#knock-name", sessionEl).value.trim());
  });
}

async function knock(sid: string, name: string): Promise<void> {
  notice = "";
  try {
    await postKnock(sid, name);
    renderWaiting(sid);
    startKnockPoll(sid);
  } catch (err) {
    notice = err instanceof ApiError && err.status === 404 ? "That session needs its full link, or no longer exists." : err instanceof Error ? err.message : String(err);
    renderKnockGate(sid);
  }
}

function renderWaiting(sid: string): void {
  newButton.hidden = false;
  setMode("home");
  placeEl.innerHTML = "";
  statusEl.innerHTML = "";
  sessionEl.innerHTML = html`<div class="card">
    <p class="section-label">${icon("clock")} Waiting</p>
    <h2>Waiting to be let in</h2>
    <p class="muted">Your request to join <code>${sid}</code> is with the session admin. This will update when they respond.</p>
  </div>`.html;
}

function startKnockPoll(sid: string): void {
  stopKnockPoll();
  knockTimer = setInterval(() => void pollKnock(sid), 3000);
}
function stopKnockPoll(): void {
  if (knockTimer !== null) {
    clearInterval(knockTimer);
    knockTimer = null;
  }
}

async function pollKnock(sid: string): Promise<void> {
  let res: { status: string; token?: string };
  try {
    res = await getKnockStatus(sid);
  } catch {
    return; // transient; the next tick retries
  }
  if (res.status === "admitted" && res.token) {
    stopKnockPoll();
    current = { sid, token: res.token, client_id: "", name: "", hasPassword: false };
    saveStored(current);
    await enterWithToken();
  } else if (res.status === "denied") {
    stopKnockPoll();
    renderDenied(sid);
  } else if (res.status === "none") {
    stopKnockPoll();
    renderKnockGate(sid); // the request was lost — ask again
  }
}

function renderDenied(sid: string): void {
  newButton.hidden = false;
  setMode("home");
  placeEl.innerHTML = "";
  statusEl.innerHTML = "";
  sessionEl.innerHTML = html`<div class="card">
    <p class="section-label">${icon("cross")} Denied</p>
    <h2>Not admitted</h2>
    <p class="warn">The session admin declined your request to join <code>${sid}</code>.</p>
    <p><a class="btn" href="${appBase}">${icon("home")} Home</a></p>
  </div>`.html;
}

function attach(s: Stored): void {
  view = initialView;
  connection = "connecting";
  clocksClosed = false;
  seenBids.clear();
  setMode("dash");
  stopEvents?.();
  stopPinging();
  stopEvents = subscribe(
    eventsURL(s.sid, "viewer"),
    s.token,
    {
      onEvent: (ev) => {
        // closed/evicted are final and snapshot-less; every other event carries a
        // snapshot (state/terminating/terminated/rejected/reopened). A warned
        // (TERMINATING) session stays live so the ping keeps running; a frozen one
        // stops it, and a reopen restarts it — syncPinger decides from the status.
        if (ev.event === "closed") {
          notice = "The session was closed.";
          stopPinging();
        } else if (ev.event === "evicted") {
          notice = "You were removed from this session.";
          stopPinging();
        } else {
          view = reduce(view, JSON.parse(ev.data) as Snapshot, Date.now());
          syncPinger();
        }
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
  pinger = new Pinger({ ping: () => doPing(s), bindActivity });
  if (ticker === null) {
    ticker = setInterval(() => {
      const now = Date.now();
      const next = tick(view, now);
      const st = view.snap?.status;
      const live = st === "OPEN" || st === "TERMINATING";
      const changed = next !== view;
      view = next;
      // Rebuild while live and progressing, or once near the hard cap so the
      // countdown and +1 h appear and tick. Otherwise a frozen session's panel
      // holds the extension form; never rebuild it on the tick (that would wipe
      // the reason input) — only patch the countdown text nodes.
      if (live && (changed || nearCap(view.snap, now))) {
        renderStatus();
      } else {
        updateClocks(now);
      }
    }, 1000);
  }
  void renderSession();
  renderStatus();
}

async function doPing(s: Stored): Promise<PingOutcome> {
  try {
    await postPing(s.sid, s.token, s.client_id);
    return "ok";
  } catch (e) {
    return e instanceof ApiError && [401, 403, 404, 409].includes(e.status) ? "stop" : "retry";
  }
}

function stopPinging(): void {
  pinger?.stop(); // stop() releases the activity binding it owns
  pinger = null;
}

// syncPinger keeps the pinger running while the session is live (OPEN or
// TERMINATING) and stops it once frozen; a reopen restarts it.
function syncPinger(): void {
  if (!current) return;
  const st = view.snap?.status;
  const live = !st || st === "OPEN" || st === "TERMINATING";
  if (live && !pinger) pinger = new Pinger({ ping: () => doPing(current!), bindActivity });
  else if (!live && pinger) stopPinging();
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

async function evict(cid: string): Promise<void> {
  if (!current || !cid) return;
  try {
    await deleteClient(current.sid, current.token, current.client_id, cid);
  } catch (err) {
    notice = `Evict failed: ${err instanceof Error ? err.message : String(err)}`;
    renderStatus();
  }
}

async function removeBeam(bid: string): Promise<void> {
  if (!current || !bid) return;
  try {
    await deleteBeam(current.sid, current.token, current.client_id, bid);
  } catch (err) {
    notice = `Remove failed: ${err instanceof Error ? err.message : String(err)}`;
    renderStatus();
  }
}

// The invite card for the active session: the QR/link to share, and the on-demand
// scanner. The link carries the token only for a public session (ADR 0020).
async function renderSession(): Promise<void> {
  newButton.hidden = current === null;
  if (!current) {
    await renderHome();
    return;
  }
  const link = joinLink(current.sid, current.token, current.hasPassword);
  sessionEl.innerHTML = html`<div class="card join">
    <p class="section-label" style="align-self:stretch">${icon("share")} Share <span class="count mono">${current.sid}</span></p>
    <canvas id="join-qr" width="256" height="256"></canvas>
    <div class="join-text">
      <p class="hint">${
        current.hasPassword
          ? "Share the id and the password — the link alone won't let anyone in."
          : "Scan the code or open the link to join on another device — everyone shares the same beams and downloads."
      }</p>
      <div class="linkrow"><code class="url">${link}</code><button class="btn small" id="copy" title="Copy link">${icon("copy")}</button></div>
      <button class="btn primary" id="scan-here" type="button">${icon("scan")} Scan a beam</button>
    </div>
  </div>`.html;
  $<HTMLButtonElement>("#copy", sessionEl).addEventListener("click", () => void navigator.clipboard?.writeText(link));
  // The scanner is opened on demand (ADR 0019): a new tab pointed at this session
  // that closes itself once the beam is received. No noopener, so it stays
  // script-closable.
  $<HTMLButtonElement>("#scan-here", sessionEl).addEventListener("click", () => window.open(scanLink(current!), "_blank"));
  try {
    await renderQR($<HTMLCanvasElement>("#join-qr", sessionEl), link);
  } catch {
    /* the link is still shown as text */
  }
}

function renderStatus(): void {
  const s = view.snap;
  if (!current) {
    placeEl.innerHTML = "";
    statusEl.innerHTML = "";
    return;
  }
  if (!s) {
    placeEl.innerHTML = html`<div class="card"><p class="muted">Connecting to the session… (${connection})</p>${
      notice ? html`<p class="warn">${notice}</p>` : ""
    }</div>`.html;
    statusEl.innerHTML = "";
    return;
  }
  const relays = `${s.relays} ${s.relays === 1 ? "relay" : "relays"}`;
  const live = s.status === "OPEN" || s.status === "TERMINATING";
  const iAmAdmin = !!current && s.clients.some((cl) => cl.client_id === current!.client_id && cl.session_admin);
  showMenu(iAmAdmin, live);
  // LEFT column: the session panel (people, requests, lifecycle).
  placeEl.innerHTML = html`
    <div class="card place${s.status === "OPEN" ? "" : " terminated"}">
      <div class="head">
        <span class="section-label" style="margin:0">${icon("beam")} Session</span>
        <span class="muted">${live ? html`<span class="livedot"></span>` : ""}${relays} · link ${connection}</span>
        ${s.status === "OPEN" ? openExpiry(s, iAmAdmin) : s.status === "TERMINATING" ? warningExpiry(s) : ""}
      </div>
      ${s.status !== "OPEN" && s.status !== "TERMINATING" ? terminatedPanel(s, Date.now()) : ""}
      <p class="section-label">${icon("people")} Participants <span class="count">${s.clients.length}</span></p>
      <ul class="clients">${s.clients.map((cl) => clientRow(cl, iAmAdmin))}</ul>
      ${
        iAmAdmin && s.knocks.length
          ? html`<p class="section-label">${icon("bell")} Requests to join <span class="count">${s.knocks.length}</span></p>
              <ul class="knocks">${s.knocks.map((k) => knockRow(k))}</ul>`
          : ""
      }
    </div>`.html;
  // RIGHT column: the beams.
  statusEl.innerHTML = html`
    <p class="section-label">${icon("beam")} Beams <span class="count">${s.beams.length}</span></p>
    ${
      s.beams.length === 0 && s.status === "OPEN"
        ? html`<div class="card"><p class="muted">Waiting — tap <b>Scan a beam</b> and point the camera at a beam page.</p></div>`
        : ""
    }
    ${view.beams.map((bv) => beamCard(bv, iAmAdmin, !seenBids.has(bv.beam.bid)))}
    ${notice ? html`<p class="warn">${notice}</p>` : ""}
  `.html;
  for (const bv of view.beams) {
    const b = bv.beam;
    seenBids.add(b.bid);
    if (b.total > 0) {
      renderChunkMarks($<HTMLElement>(`#grid-${b.bid}`, statusEl), b.bid, decodeBitmap(b.bitmap, b.total));
    }
  }
  // Beam actions live in the right column; everything else in the left.
  statusEl.querySelectorAll<HTMLButtonElement>("[data-download]").forEach((btn) =>
    btn.addEventListener("click", () => void download(btn.dataset.beam ?? "", btn.dataset.download ?? "raw")),
  );
  statusEl.querySelectorAll<HTMLButtonElement>("[data-remove-beam]").forEach((btn) =>
    btn.addEventListener("click", () => void removeBeam(btn.dataset.removeBeam ?? "")),
  );
  placeEl.querySelectorAll<HTMLButtonElement>("[data-evict]").forEach((btn) =>
    btn.addEventListener("click", () => void evict(btn.dataset.evict ?? "")),
  );
  placeEl.querySelectorAll<HTMLButtonElement>("[data-admit]").forEach((btn) =>
    btn.addEventListener("click", () => void resolveKnockClick(btn.dataset.admit ?? "", "admit")),
  );
  placeEl.querySelectorAll<HTMLButtonElement>("[data-deny]").forEach((btn) =>
    btn.addEventListener("click", () => void resolveKnockClick(btn.dataset.deny ?? "", "deny")),
  );
  placeEl.querySelector<HTMLFormElement>("#ext-form")?.addEventListener("submit", onExtensionSubmit);
  placeEl.querySelector<HTMLButtonElement>("#reopen-btn")?.addEventListener("click", onReopen);
  placeEl.querySelector<HTMLButtonElement>("#extend-btn")?.addEventListener("click", onExtend);
}

// onEndSession soft-terminates: the session freezes but its downloads stay for the
// terminated window before the sweep removes them.
function onEndSession(): void {
  if (!current) return;
  if (!confirm("End this session? Downloads stay available for a while, then it is removed.")) return;
  void deleteSession(current.sid, current.token, current.client_id, false).catch((err) => {
    notice = `Could not end the session: ${err instanceof Error ? err.message : String(err)}`;
    renderStatus();
  });
}

// onHardDelete purges the session and its files at once (ADR 0019), then drops to
// the create screen.
function onHardDelete(): void {
  if (!current) return;
  if (!confirm("Permanently delete this session and its files now? This cannot be undone.")) return;
  const cur = current;
  void deleteSession(cur.sid, cur.token, cur.client_id, true)
    .then(() => {
      stopEvents?.();
      stopEvents = null;
      stopPinging();
      clearStored(cur.sid);
      location.href = appBase; // the session is gone: back to the home page
    })
    .catch((err) => {
      notice = `Could not delete the session: ${err instanceof Error ? err.message : String(err)}`;
      renderStatus();
    });
}

// onReopen revives a session suspended by inactivity: registering reopens it
// server-side (ADR 0018) and the SSE then pushes OPEN, restarting the pinger.
function onReopen(): void {
  if (!current) return;
  const btn = placeEl.querySelector<HTMLButtonElement>("#reopen-btn");
  if (btn) {
    btn.disabled = true;
    btn.textContent = "Reopening…";
  }
  void registerClient(current.sid, current.token, { role: "viewer", resume: current.client_id || undefined })
    .then((c) => {
      if (current) current.client_id = c.client_id;
    })
    .catch((err) => {
      if (btn) {
        btn.disabled = false;
        btn.textContent = "Reopen session";
      }
      notice = `Could not reopen: ${err instanceof Error ? err.message : String(err)}`;
      renderStatus();
    });
}

// onExtend grants the session another hour before the max_age cap (session admin).
function onExtend(): void {
  if (!current) return;
  const btn = placeEl.querySelector<HTMLButtonElement>("#extend-btn");
  if (btn) btn.disabled = true;
  void postExtendMaxAge(current.sid, current.token, current.client_id)
    .catch((err) => {
      notice = `Could not extend: ${err instanceof Error ? err.message : String(err)}`;
      renderStatus();
    })
    .finally(() => {
      const b = placeEl.querySelector<HTMLButtonElement>("#extend-btn");
      if (b) b.disabled = false;
    });
}

const EXPIRY_SOON_MS = 30 * 60 * 1000; // show the countdown only near the hard cap

/** True when an OPEN session is within 30 min of its hard cap. Presence keeps a
 *  connected session alive until then, so there is no countdown to show (ADR 0019). */
function nearCap(s: Snapshot | null, now: number): boolean {
  if (!s || s.status !== "OPEN") return false;
  const at = instantMs(s.expires_at);
  return at !== null && at - now <= EXPIRY_SOON_MS;
}

/** The "ends in …" countdown, shown only in the last 30 min before the hard cap;
 *  a session admin gets a +1 h button alongside to push the cap out (ADR 0019). */
function openExpiry(s: Snapshot, iAmAdmin: boolean): Raw {
  if (!nearCap(s, Date.now())) return raw("");
  const c = expiryCountdown(s, Date.now());
  return html`<span class="muted expiry">ends in <span id="expiry" class="clock">${c.text}</span></span>${
    iAmAdmin ? html` <button class="btn small" id="extend-btn" type="button" title="Push the limit out by an hour">+1 h</button>` : ""
  }`;
}

/** The warning countdown while TERMINATING (an airlift admin is ending it). */
function warningExpiry(s: Snapshot): Raw {
  const c = terminateCountdown(s, Date.now());
  return html`<span class="expiry warn">ending in <span id="expiry" class="clock">${c.hidden ? "…" : c.text}</span></span>`;
}

/** The panel for a non-live session: who/why, a cleanup countdown, and — while
 *  TERMINATED — an extension request; PENDING_REVIEW shows the awaiting-review
 *  note, REJECTED the reviewer's note. */
function terminatedPanel(s: Snapshot, now: number): Raw {
  if (s.reopenable) {
    // Suspended by inactivity (ADR 0018): reopen it from the link, no review needed.
    return html`<div class="ended">
      <p class="ended-head"><strong>Session paused.</strong> It closed after a spell of inactivity.</p>
      <p>Reopen it to carry on — every timer resets.</p>
      <p><button id="reopen-btn" class="btn primary" type="button">${icon("reopen")} Reopen session</button></p>
    </div>`;
  }
  const t = s.terminated;
  if (s.status === "PENDING_REVIEW") {
    return html`<div class="ended">
      <p class="ended-head"><strong>Awaiting review.</strong> A request for more time is with the tower's administrator.</p>
      ${s.extension?.reason ? html`<p class="muted">“${s.extension.reason}”</p>` : ""}
    </div>`;
  }
  const c = t ? cleanupCountdown(t, now) : { text: "", done: true, hidden: true };
  const cleanupLine = c.hidden
    ? html`<p>The session has ended.</p>`
    : c.done
      ? html`<p>The session has closed and its files have been removed.</p>`
      : html`<p>Downloads stay available for another <span id="cleanup" class="clock">${c.text}</span>.</p>`;
  return html`<div class="ended">
    <p class="ended-head"><strong>${t ? terminatedBy(t) : "Session ended"}.</strong> ${t ? terminatedWhy(t) : ""}</p>
    ${s.status === "REJECTED" && s.extension?.note ? html`<p class="muted">Note: ${s.extension.note}</p>` : ""}
    ${cleanupLine}
    ${s.status === "TERMINATED" && !c.done
      ? html`<form id="ext-form" class="ext-form">
          <input id="ext-reason" type="text" placeholder="why you need more time (optional)" />
          <button class="btn small primary" type="submit">Request more time</button>
        </form>`
      : ""}
  </div>`;
}

function onExtensionSubmit(e: Event): void {
  e.preventDefault();
  if (!current) return;
  const reason = placeEl.querySelector<HTMLInputElement>("#ext-reason")?.value.trim() ?? "";
  const btn = placeEl.querySelector<HTMLButtonElement>("#ext-form button");
  if (btn) btn.disabled = true;
  void postExtension(current.sid, current.token, current.client_id, reason).catch((err) => {
    notice = `Could not request more time: ${err instanceof Error ? err.message : String(err)}`;
    if (btn) btn.disabled = false;
    renderStatus();
  });
}

// updateClocks patches only the countdown text nodes so the download/evict/remove
// buttons are never rebuilt mid-click; a full re-render happens only on the
// one-shot swap to the "closed" line.
function updateClocks(now: number): void {
  const s = view.snap;
  if (!current || !s) return;
  if (s.status === "OPEN") {
    const c = expiryCountdown(s, now);
    const el = placeEl.querySelector<HTMLElement>("#expiry");
    if (el && !c.hidden) el.textContent = c.text;
  } else if (s.status === "TERMINATING") {
    const c = terminateCountdown(s, now);
    const el = placeEl.querySelector<HTMLElement>("#expiry");
    if (el && !c.hidden) el.textContent = c.text;
  } else if ((s.status === "TERMINATED" || s.status === "REJECTED") && s.terminated) {
    // Only these two have a live cleanup countdown; PENDING_REVIEW's is frozen.
    const c = cleanupCountdown(s.terminated, now);
    if (c.done) {
      if (!clocksClosed) {
        clocksClosed = true;
        renderStatus(); // swap the panel to the "removed" line once, without a loop
      }
      return;
    }
    const el = placeEl.querySelector<HTMLElement>("#cleanup");
    if (el) el.textContent = c.text;
  }
}

/** One pending admission request; a session admin admits or denies it (ADR 0021). */
function knockRow(k: KnockView): Raw {
  return html`<li>
    <span class="who">${k.name || "(anonymous)"}</span>
    <button class="btn small primary" data-admit="${k.id}">${icon("check")} Admit</button>
    <button class="btn small" data-deny="${k.id}">${icon("cross")} Deny</button>
  </li>`;
}

async function resolveKnockClick(kid: string, decision: "admit" | "deny"): Promise<void> {
  if (!current || !kid) return;
  try {
    await resolveKnock(current.sid, current.token, current.client_id, kid, decision);
  } catch (err) {
    notice = `Could not ${decision} the request: ${err instanceof Error ? err.message : String(err)}`;
    renderStatus();
  }
}

/** One client in the place's people list; admins get an Evict button on others. */
function clientRow(cl: ClientSummary, iAmAdmin: boolean): Raw {
  const me = !!current && cl.client_id === current.client_id;
  const tags: string[] = [];
  if (cl.session_admin) tags.push("admin");
  tags.push(...cl.roles);
  if (me) tags.push("you");
  return html`<li class="${cl.connected ? "on" : "off"}">
    <span class="who">${cl.name}</span>${tags.length ? html` <span class="tags">${tags.join(" · ")}</span>` : ""}
    ${iAmAdmin && !me ? html` <button class="btn small" data-evict="${cl.client_id}">${icon("cross")} Evict</button>` : ""}
  </li>`;
}

/** One beam's card: progress, then a verified or failed panel once terminal.
 *  A card seen for the first time rises in. */
function beamCard(bv: BeamView, iAmAdmin: boolean, fresh: boolean): Raw {
  const b = bv.beam;
  return html`<div class="card beam${fresh ? " enter" : ""}" data-bid="${b.bid}">
    <div class="head">
      <span class="badge" data-state="${b.state}">${STATE_LABELS[b.state]}</span>
      <strong>${b.name || "(unnamed)"}</strong>
      <span class="muted">beam ${b.bid}</span>
      ${iAmAdmin ? html`<button class="btn small" data-remove-beam="${b.bid}">${icon("trash")} Remove</button>` : ""}
    </div>
    <div class="progress">
      <div class="big">${b.total > 0 ? `${b.have} / ${b.total}` : "— / —"}</div>
      <div id="grid-${b.bid}" class="chunks" ${b.total > 0 ? "" : raw("hidden")}></div>
      <div class="metrics">
        <span><b>${b.fps.toFixed(1)}</b> fps decoded</span>
        <span>elapsed <b>${formatDuration(bv.elapsedMs)}</b></span>
        <span>ETA <b>${bv.etaSec === null ? "—" : formatDuration(bv.etaSec * 1000)}</b></span>
      </div>
    </div>
    ${b.state === "READY" ? resultCard(b) : ""}
    ${b.state === "FAILED" ? failedCard(b) : ""}
  </div>`;
}

function verdictRow(label: string, v: Verdict | null): Raw {
  if (!v) return raw("");
  return html`<tr class="${v.ok ? "ok" : "bad"}">
    <th>${label}</th>
    <td class="mark">${v.ok ? icon("check") : icon("cross")}</td>
    <td><div>expected <code>${v.expected}</code></div><div>actual <code>${v.actual}</code></div></td>
  </tr>`;
}

function resultCard(b: Beam): Raw {
  const v = b.verdicts;
  return html`<div class="result ok">
    <h3>${icon("check")} Verified</h3>
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
        (d) => html`<button class="btn primary" data-beam="${b.bid}" data-download="${d}">${icon("download")} ${DOWNLOAD_LABELS[d] ?? d}</button>`,
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
    <h3>${icon("cross")} Failed</h3>
    <p>${b.error ?? "Verification failed."}</p>
    ${
      stage && v
        ? html`<p><b>${STAGE_LABELS[stage]}</b> did not match.</p>
            <table class="verdicts"><tr><th>expected</th><td colspan="2"><code>${v.expected}</code></td></tr><tr><th>actual</th><td colspan="2"><code>${v.actual}</code></td></tr></table>`
        : ""
    }
  </div>`;
}

// "New session" simply returns to the home page, where you can create or join one.
newButton.addEventListener("click", () => {
  location.href = appBase;
});
// The power menu: toggle on the button, close on an action or a click elsewhere.
powerBtn.addEventListener("click", (e) => {
  e.stopPropagation();
  const open = menuEl.classList.toggle("open");
  powerBtn.setAttribute("aria-expanded", String(open));
});
menuEl.addEventListener("click", (e) => e.stopPropagation());
document.addEventListener("click", closeMenu);
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeMenu();
});
endBtn.addEventListener("click", () => {
  closeMenu();
  onEndSession();
});
delBtn.addEventListener("click", () => {
  closeMenu();
  onHardDelete();
});
bindCopyButtons(document);
bindCloneTabs();
void boot();
