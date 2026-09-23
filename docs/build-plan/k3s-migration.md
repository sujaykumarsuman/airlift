# Build plan — migrating the tower to k3s

Status: **executed 2026-09-15** (originally a plan; the migration below was
carried out — the tower now runs on k3s).

> **Now GitOps-managed (updated 2026-09-15):** the cluster is reconciled by Flux +
> Helm from the `sujaykumarsuman/infra` repo. The `deploy/k8s/` manifests and the
> `make k3s-*` targets this document references were removed from the airlift repo
> and reworked as a shared Helm chart there. This doc stays as the record of the
> initial (kubectl-applied) migration.
>
> **Storage updated (2026-09-23):** the PVC moved from k3s `local-path` to Longhorn
> (`longhorn-static`, Delete-reclaim), consolidating all persistent data onto
> Longhorn. The `local-path` references below are the original migration's choice;
> the volume is now Longhorn-backed. Behaviour is otherwise identical — a single RWO
> writer with `Recreate`, sessions still emptied on start.

Prepared alongside the wider move of
`projects.sujaykumar.dev` from a Caddy + systemd host to a single-node **k3s**
cluster, with each project in its own namespace. This document is about the
**airlift tower** specifically: what has to change to run it on Kubernetes, why
a naive multi-replica deployment breaks it, and the phased path from the current
single-process service to a distributed one — should that ever be wanted.

The infrastructure decisions (taken 2026-09-15) that frame everything below:

- **Routing: path prefixes kept.** The tower stays at
  `https://projects.sujaykumar.dev/airlift`; Traefik strips `/airlift` before it
  reaches the pod, exactly as Caddy does today (ADR 0012). `public_url` is
  unchanged, so the `<base href>` machinery and every emitted link keep working.
- **TLS: Traefik + cert-manager, inside k3s.** The bundled Traefik terminates
  TLS with a Let's Encrypt certificate issued by cert-manager (HTTP-01). Host
  Caddy is retired at cutover.
- **Cutover: prepared now, flipped together.** All manifests, the image build,
  the deploy tooling and this plan are prepared ahead of time; the k3s install
  and the 80/443 flip are done as one deliberate, watched step because this is a
  live, single, SSH-only node with no console fallback.

---

## 1. Goals and constraints

**Goal.** Run the tower as a Kubernetes workload in the `airlift` namespace on
k3s, reachable at the same public URL, with no loss of the properties it has
today: real client IP, TLS, on-disk beam persistence within a session's life,
the admin surface, and runtime config overrides.

**Constraints that shape the design:**

- **One node, small.** 1 vCPU, 3.8 GiB RAM, 48 GB disk. k3s' own control plane
  plus Traefik plus cert-manager already claims a meaningful slice of that
  vCPU/RAM; the tower must stay as lean as it is now.
- **Sessions are memory-only by design** (ADR 0005, and an explicit non-goal in
  `CLAUDE.md`: *"Persistence across tower restarts. Sessions are memory-only;
  `data_dir` is emptied on start."*). A pod restart dropping in-flight transfers
  is already the accepted behaviour — the systemd `restart` on every `make
  deploy` does exactly this today.
- **The tower is stateful and single-writer.** Session state, per-beam decoders,
  the SSE fan-out and the beam files on disk all live inside one process. This
  is the crux of the whole document.

**Explicit non-goal for this migration:** turning the tower into a
horizontally-scaled service. On a single node there is nothing to scale to.
Phase B below exists so the constraint is understood and the door is left open;
it is **not** scheduled work.

---

## 2. Cluster context (shared infrastructure)

The tower's manifests assume the cluster below. It is set up once and reused by
every future project; the concrete files live in `deploy/k8s/cluster/`.

| Piece | Choice | Notes |
| --- | --- | --- |
| Distribution | k3s (single server node) | bundled Traefik kept; ServiceLB (klipper) binds Traefik to host `:80`/`:443` |
| Ingress | Traefik (CRDs: `IngressRoute`, `Middleware`, `TLSStore`) | one hostname, path-prefix routing across namespaces |
| TLS | cert-manager + Let's Encrypt (HTTP-01 via Traefik) | one `Certificate` for `projects.sujaykumar.dev` in `kube-system`; a Traefik `TLSStore/default` points every route at it, so per-namespace routes need no own secret |
| Namespaces | one per project (`airlift`, `projects-hub`, …) | workloads isolated; routes for the shared host live beside each workload |
| Images | no registry — `docker save … \| k3s ctr images import -` | a single node; a registry is unnecessary overhead |
| Volumes | k3s `local-path` provisioner (RWO, node-local) | fine for a single node; do not assume RWX |

**Why a `TLSStore/default` and not a secret per route.** With path-prefix
routing, several namespaces answer for the *same* host
(`projects.sujaykumar.dev`). A Traefik `IngressRoute` with TLS normally needs the
certificate secret in its own namespace, which would mean issuing (and renewing)
the same certificate two or more times. Instead cert-manager issues it once into
`kube-system`, a `TLSStore` named `default` (in Traefik's namespace) adopts it as
the default certificate, and each project's `IngressRoute` sets `tls: {}` — no
secret reference, no duplication.

**Client-IP preservation (do not skip this).** Today the tower sees the real
client address because Caddy sits on `127.0.0.1` and forwards
`X-Forwarded-For`, which the tower reads right-to-left against `trusted_proxies`
(ADR 0012). Under k3s this must be re-established deliberately:

1. Set the Traefik service `externalTrafficPolicy: Local` (via a
   `HelmChartConfig`), so ServiceLB does not SNAT the source address before
   Traefik sees it.
2. Traefik then sets `X-Forwarded-For` to the real client IP.
3. The tower must **trust Traefik's pod address** — set `trusted_proxies` to the
   k3s pod CIDR (`10.42.0.0/16` by default), not `127.0.0.1`.

Miss any of the three and the admin surface, rate limits and eviction all key
off Traefik's IP instead of the caller's.

---

## 3. The tower today: an inventory of process-local state

Every item below currently lives inside the one tower process (in memory, or on
that pod's local disk). This is the list a distributed design has to account
for. References are to the ADRs in `docs/adr/`.

| State | Where it lives now | ADR |
| --- | --- | --- |
| Sessions (the "places") and their beams | in-memory `Store`; source of truth | 0005, 0015 |
| Per-beam decoder state (chunk buckets, fountain solver, pre-manifest hold) | in-memory, per beam, per sender | 0015 |
| Verified beam files (`raw/`, `tree/`, `<stem>.zip`, `meta.json`) | pod-local disk under `data_dir` | 0016 |
| Downloads | streamed from that pod's disk (`http.ServeContent`) | 0016 |
| Client registry (per-device identity, resume keys, presence) | in-memory, per session | 0017, 0022 |
| SSE downstream (snapshot diffs to every watcher) | in-process fan-out; each client holds a streaming fetch | 0007, 0010 |
| Rate limiters (create / join / frames / ping / admin) | in-memory token buckets, per address/session | 0017 |
| Lifecycle clocks + the two-phase sweep | in-memory timers / a goroutine | 0013, 0018, 0019 |
| Knocks (admission requests) | in-memory, per session, address-keyed | 0021 |
| Upload approvals (direct-send consent) | in-memory, per session, address-keyed | 0023 |
| Runtime config overrides | `srv.live` atomic swap + a pod-local `overrides` file | 0014 |
| Admin token | `AIRLIFT_ADMIN_TOKEN` env (already externalisable) | 0014 |

The only two items that are already deployment-friendly are the beam files
(local disk, and wiped on start anyway) and the admin token (an env var).
Everything else assumes a single process is the sole owner and reader.

---

## 4. Why more than one replica breaks it

Put two replicas behind the Service and the in-memory model splits in half. A
concrete failure, all of which happen with round-robin load-balancing:

- A scanner opens its **SSE** stream on pod A and watches a session; another
  client **POSTs frames** for the same session to pod B. Pod B has never heard
  of that session (or has a different copy of it), so the frames are rejected or
  land in a second, invisible beam; pod A's watcher never sees progress.
- A beam reaches **READY** and is written to pod B's disk; the **download**
  request round-robins to pod A, which has no such file → 404.
- **Rate limits** and **eviction** are per-pod, so limits are effectively
  doubled and an evicted address is still served by the other pod.
- An admin **config override** (ADR 0014) is applied on pod A only; pod B keeps
  the old value. The admin **sessions table** shows half the sessions depending
  on which pod answered.

None of this is a bug to fix in place — it is the direct consequence of ADR 0005
("the server is the source of truth") being satisfied by *one* server. Running
more than one requires either pinning each session to a pod (§6, B1) or moving
the source of truth out of the process (§6, B2).

---

## 5. Phase A — lift to k3s as a single replica (the actual move)

This is what goes to production. **No tower code changes.** The tower runs as a
one-replica Deployment; correctness is preserved because there is still exactly
one process that owns the state.

Manifests: `deploy/k8s/airlift/` (kustomize). Summary:

- **Deployment**, `replicas: 1`, **`strategy: Recreate`**. Recreate (not
  RollingUpdate) is deliberate: the pod holds a RWO PVC and is the single writer
  of session state, so the old pod must fully stop before the new one starts.
  The few seconds of unavailability on a deploy is identical to today's systemd
  restart and is covered by the memory-only non-goal.
- **PVC** (`local-path`, RWO) mounted at `AIRLIFT_HOME` (`/var/lib/airlift/.airlift`).
  Sessions do not survive a restart regardless (`data_dir` is emptied on start),
  so the PVC exists only to persist the small `overrides` file (runtime admin
  config) and the config across restarts. An `emptyDir` is a valid lighter
  alternative if losing runtime overrides on restart is acceptable.
- **ConfigMap** → mounted as `AIRLIFT_HOME/config` (subPath). Holds the three
  keys that must change for k8s (see §7).
- **Secret** → `AIRLIFT_ADMIN_TOKEN` via `envFrom`. The value is **not**
  committed; it is created from the token already live on the VPS (see the
  cutover runbook) so the admin token does not rotate.
- **Service** (ClusterIP) → `8443`.
- **Middleware** (`StripPrefix: /airlift`) + a redirect `/airlift` → `/airlift/`
  (the 308 Caddy does today).
- **IngressRoute**, host `projects.sujaykumar.dev`, `PathPrefix(\`/airlift\`)`,
  the strip middleware, `tls: {}` (default store). A higher `priority` than the
  hub's catch-all so `/airlift/*` wins.
- **Probes**: `httpGet /api/info` (public, ADR 0012) for readiness and liveness.
  The image is distroless (no shell), so exec probes are not an option — this is
  why `/api/info` being public matters.
- **securityContext**: `runAsNonRoot`, uid/gid `65532` (distroless nonroot),
  `fsGroup: 65532` so the `local-path` volume is writable. Fallback if `fsGroup`
  is not honoured: an initContainer that `chown`s the mount.
- Optional **NetworkPolicy**: allow ingress to `:8443` only from the Traefik
  pod, since the container now listens on `0.0.0.0` (see §7).

Resource envelope (starting point, tune from `kubectl top`): requests
`50m` / `64Mi`, limits `250m` / `128Mi`.

---

## 6. Phase B — distributed architecture (deferred)

Only needed for horizontal scale, multi-node, or genuine zero-downtime rollouts.
On the current single node it buys nothing, so it is documented, not scheduled.
Note that **rolling updates alone do not need Phase B but also cannot be
zero-downtime without it**: even with session affinity, the draining pod's
in-memory sessions die when it terminates. Honest zero-downtime requires the
state to outlive any one pod — i.e. B2.

### B1 — Session affinity (shard, share nothing)

Keep the in-process model; make sure every request for a session reaches the one
pod that owns it. The session id is already in the path
(`/<sid>/…`, ADR 0020), which is the natural sharding key.

- **Routing.** Consistent-hash `<sid>` → pod. Traefik cannot content-hash to a
  specific backend pod out of the box; realistic options are a StatefulSet with
  stable per-pod Services and a small routing shim (or Traefik cookie-based
  sticky sessions, which pins by client not by session and so breaks the moment
  a *second* device joins the *same* session on a different pod — a real airlift
  case, ADR 0022, so cookie stickiness is not sufficient).
- **Pros:** minimal tower code change; each session keeps its fast in-process
  decoder and SSE.
- **Cons:** the routing shim is the new hard part; rebalancing on scale-up moves
  (i.e. drops) live sessions; a pod restart still drops its shard; the admin
  view must fan out across pods and merge. Verdict: cheap code, expensive and
  fiddly plumbing, and it does not deliver zero-downtime.

### B2 — Externalised state (true horizontal scale)

Move the source of truth out of the process. Each item from §3, and where it
would go:

- **Session / client / lifecycle / knocks / upload metadata** → a shared store.
  Redis (with TTLs mapping cleanly onto the lifecycle clocks) or Postgres.
  Define a schema keyed by `sid`, with beams keyed by `sid+bid`.
- **Beam files** → shared object storage: MinIO in-cluster (S3 API) or a RWX
  volume. Downloads stream from there; `session.Download`'s existing `Blob` seam
  (`MemBlob`/`fileBlob`, ADR 0016) is the right place to add an `s3Blob`.
- **SSE fan-out** → a pub/sub bus (Redis pub/sub or NATS). A state change on any
  pod publishes on `sid`; any pod with a watcher subscribed re-renders the
  snapshot diff. This decouples "who holds the SSE stream" from "who changed the
  state".
- **Beam frame ingestion / decode — the genuinely hard part.** A decoder holds
  in-memory chunk/fountain state that mutates on every frame batch. Three ways:
  1. **Persist partial state per batch** to the shared store so any pod can pick
     a beam up. Correct but chatty and CPU-heavy — it fights the fast path.
  2. **Route all frames for a `sid+bid` to the pod that owns it** (a queue or a
     consistent-hash), i.e. re-introduce affinity but only for the hot
     ingestion path while metadata/files/SSE are shared.
  3. Pin ingestion to one pod outright; accept a beam is lost if that pod dies
     mid-transfer (already the accepted behaviour today).
  **Recommended hybrid:** shared metadata + shared files + shared SSE bus, with
  **per-beam ingestion affinity** (option 2/3). The pod that receives a beam's
  MANIFEST records its own address in the session record; frames for that
  `sid+bid` are routed there; everything else any pod can serve.
- **Rate limiters** → shared token buckets in Redis (or accept per-pod
  approximations, which are usually fine).
- **Runtime overrides (ADR 0014)** → move from the pod-local `overrides` file to
  the shared store (or a watched ConfigMap) so a PATCH applies cluster-wide
  instead of on one pod.
- **Admin token** → already a Secret; nothing to do.

**Surface.** This touches `internal/session` (the store becomes an interface
with an external backend), `internal/server` (SSE off a bus; frame routing;
downloads via an S3 blob; shared limiters; overrides from the store), and
`internal/config`. It is a substantial, ADR-worthy change (it would supersede
the single-owner assumption of ADR 0005). It also adds Redis/MinIO pods — real
memory on a box that has little to spare.

### Recommendation

Ship **Phase A**. Treat **Phase B** as future work, and if it is ever taken up,
do the **hybrid B2** (shared metadata/files/SSE + per-beam ingestion affinity)
rather than B1's routing shim or a fully re-entrant decoder. Write it up as a new
ADR superseding 0005 when the time comes.

---

## 7. Config changes required for k8s

Three keys differ from the VPS config today. They go in the ConfigMap
(`deploy/k8s/airlift/configmap.yaml`):

| Key | VPS today | k8s value | Why |
| --- | --- | --- | --- |
| `listen` | `127.0.0.1:8443` | `0.0.0.0:8443` | Traefik is a *different* pod; the container must accept connections on the pod network, not loopback |
| `trusted_proxies` | default (`127.0.0.1`) | `10.42.0.0/16` | trust `X-Forwarded-For` from Traefik, which runs in the k3s pod CIDR |
| `public_url` | `https://projects.sujaykumar.dev/airlift` | *unchanged* | path prefixes kept; base-href and links stay valid |

`0.0.0.0` listening is safe because the pod is only reachable through the Service
(and, optionally, a NetworkPolicy restricting `:8443` to Traefik). The admin
token stays in a Secret, never the ConfigMap.

**Prerequisite — `trusted_proxies` must accept a CIDR (done on this branch).**
Traefik's pod address is dynamic (somewhere in `10.42.0.0/16`), so it cannot be
pinned as a bare IP. The config previously accepted only bare IPs
(`netip.ParseAddr`); it now also accepts CIDRs (`internal/config/registry.go`
validation + `internal/server/proxy.go` `ParseTrustedProxies`, with tests in
`config_test.go` and `server_test.go`). The runtime trust check already matched
on prefixes (`proxy.go` `p.Contains`), so this is validation/parsing only. Build
the migration image from a checkout that includes this change — without it the
tower would reject the k8s config and fall back to seeing every request as
Traefik's IP, collapsing the per-address rate limits, eviction, knocks and
uploads onto one identity.

---

## 8. Image build and distribution (no registry)

Multi-stage `deploy/Dockerfile` mirrors the Makefile exactly (npm build → embed
`web/dist` → `CGO_ENABLED=0` Go build with the `Version` ldflag), on top of
`gcr.io/distroless/static-debian12:nonroot`. Built on the VPS (it already has
Docker and Go) from a clean checkout, then imported into k3s' containerd:

```
git clone https://github.com/sujaykumarsuman/airlift && cd airlift
V=$(git describe --tags)
docker build -f deploy/Dockerfile --build-arg VERSION=$V -t airlift:$V -t airlift:latest .
docker save airlift:$V airlift:latest | k3s ctr images import -
```

The Deployment pins `airlift:latest` with `imagePullPolicy: IfNotPresent` (the
image is pre-imported, never pulled); a redeploy re-imports `latest` and
`kubectl rollout restart` picks it up. The binary is stamped with the real
`VERSION` via the ldflag, so `GET /api/info` reports the true version regardless
of the image tag. `make image` / `make k8s-deploy` wrap this (see the Makefile).
A registry is deliberately avoided on a single node.

---

## 9. Cutover runbook (watched)

Ordered so the site is down for the shortest possible window and so a failure at
any step can roll back to Caddy + systemd. **Capture the live admin token first**
so it is not rotated.

**Pre-flight (no downtime):**

1. `TOKEN=$(ssh <vps> "sed -n 's/^AIRLIFT_ADMIN_TOKEN=//p' /etc/default/airlift")`
   — keep it for step 7.
2. Clone the repo on the VPS; build and import the image (§8).
3. Confirm the DNS A record for `projects.sujaykumar.dev` still points at the VPS
   (it does; unchanged).

**Flip (brief downtime — this is the watched part):**

4. `systemctl disable --now caddy airlift` — frees `:80`/`:443` and stops the
   old tower. **Downtime starts here.**
5. Install k3s (`deploy/k8s/cluster/install-k3s.sh`); wait for the node Ready and
   Traefik/ServiceLB to bind `:80`/`:443`.
6. Apply cluster infra (`deploy/k8s/cluster/bootstrap.sh`): cert-manager, the
   ClusterIssuer, the `Certificate` for the host, the `TLSStore/default`, the
   Traefik `HelmChartConfig` (externalTrafficPolicy Local), and the namespaces.
   Wait for the `Certificate` to be **Ready** (use the Let's Encrypt **staging**
   issuer first to avoid rate limits, confirm the chain, then switch to prod).
7. Create the admin Secret from `$TOKEN`; `kubectl apply -k deploy/k8s/airlift`
   and the hub manifests (in the `sujaykumarsuman.github.io` repo).
8. Verify (see below). **Downtime ends when verification passes.**

**Verify:**

- `curl https://projects.sujaykumar.dev/airlift/api/info` → 200, correct
  `version` (`v0.1.8`).
- `curl https://projects.sujaykumar.dev/` → the hub page.
- The certificate is the prod Let's Encrypt one (not staging, not self-signed).
- Create a session; confirm the tower logs the **real** client IP (not
  `10.42.x.x`) — proves §2's client-IP chain.
- Drive a beam to READY (or run `internal/replay` against it) and download it.
- `/airlift/admin` with `$TOKEN`.

**Rollback (if any verify step fails):** `kubectl delete` the airlift resources
(or scale to 0), `systemctl enable --now caddy airlift`. The old path is intact
until step 10 of cleanup removes it.

---

## 10. Residue cleanup (after cutover is verified and has soaked)

Do **not** run these until the k3s path has served real traffic for a day or so
— they remove the rollback path. Precise list, each marked reversible (♻) or
irreversible (⛔):

- ⛔ `systemctl disable --now airlift` and remove `/etc/systemd/system/airlift.service`,
  `/usr/local/bin/airlift`, `/etc/default/airlift` (token now a Secret), and
  `/var/lib/airlift` (wiped on start anyway).
- ⛔ Retire Caddy: `systemctl disable --now caddy`, `apt-get purge caddy`, remove
  `/etc/caddy`. (TLS is Traefik's now.)
- ♻ **Docker + containerd.io** are **kept** (not removed at cutover): they were
  idle residue before, but are now the image build tool — `make k3s-image` runs
  `docker build` on the VPS and imports into k3s' own containerd. Purge only if
  you move image builds off the box (then `apt-get purge docker-ce docker-ce-cli
  containerd.io`).
- ⛔ Remove the default `/var/www/html` and the old `/var/www/projects` webroot
  (the hub is a ConfigMap in k3s now).
- ⛔ The careerdock backup was **tidied out of `/root` into
  `/root/backups/careerdock-20260910/`** during prep (checksum re-verified; a
  verified copy also exists on the laptop at `~/Backups/careerdock-20260910/`).
  It may be deleted from the VPS entirely once you are content the laptop copy
  suffices — that reclaims ~666 MB (the tar plus the extracted tree).

---

## 11. Risks and open questions

- **iptables over SSH.** k3s rewrites iptables. It does not touch `:22`, but a
  botched install on a console-less box is the sharpest risk — hence the watched
  cutover and the Caddy/systemd rollback kept until soak.
- **Resource headroom.** k3s + Traefik + cert-manager on 1 vCPU / 3.8 GiB is
  workable but not generous. Watch `kubectl top nodes`; if it is tight, the
  in-cluster additions of Phase B (Redis/MinIO) are a further reason to defer it.
- **HTTP-01 renewal vs the catch-all route.** cert-manager's ACME solver serves
  `/.well-known/acme-challenge/*`; the hub's `PathPrefix(\`/\`)` must not shadow
  it. Traefik prioritises the solver's exact path, and the first issue happens
  before the app routes exist (runbook order), but confirm a renewal succeeds
  with the app routes live before trusting it.
- **`local-path` is node-local and RWO.** Fine now; a hard blocker for any
  multi-node future (Phase B assumes shared/object storage instead).
- **Open question:** is a zero-downtime deploy ever actually wanted here? If not,
  Phase A's Recreate is the honest, permanent answer and Phase B never needs to
  happen.
