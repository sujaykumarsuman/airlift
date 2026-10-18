# Hosting airlift

The live tower runs on a single-node **k3s** cluster and is deployed by **GitOps**
— Flux + Helm reconcile it from the [`sujaykumarsuman/infra`](https://github.com/sujaykumarsuman/infra)
repo. Nothing is deployed by hand from this repo; `make` only builds.

It is served at **`https://projects.sujaykumar.dev/airlift`**; the admin console
is on the ops host, **`https://ops.sujaykumar.dev/airlift/admin`** (on `projects`,
`/airlift/admin` and `/airlift/api/admin/*` answer 403). Traefik terminates TLS
with a Let's Encrypt certificate (cert-manager) and strips the `/airlift` prefix
before the pod, so the tower's router stays rooted while `public_url` carries the
prefix and the tower injects `<base href="/airlift/">` (ADR 0012).

## How a change reaches production

```
git tag vX.Y.Z            # cut a release
  → .github/workflows/deploy.yml builds ghcr.io/sujaykumarsuman/airlift:X.Y.Z
      → a PR in sujaykumarsuman/infra sets the tag in apps/airlift.yaml (by hand)
          → helm-controller upgrades the release → the pod is recreated on k3s
```

Pull-based: the cluster is never exposed to CI. Tagging builds the image but
deploys nothing: since 2026-10-18 Flux still resolves the newest tag (the
`airlift` ImagePolicy) but no longer writes it into `apps/airlift.yaml`. The tag is
bumped by hand, merged only after `hack/airlift-idle.sh` reports that no beam is
mid-transfer — the infra README's "Deploying airlift" has the steps.

## What runs where

| Piece | Location |
| --- | --- |
| container image | `ghcr.io/sujaykumarsuman/airlift` (built from `deploy/Dockerfile`) |
| Deployment / Service / route | rendered by the shared `project` Helm chart in `sujaykumarsuman/infra` (`apps/airlift.yaml`); strategy `Recreate` |
| config (`public_url`, `listen=0.0.0.0:8443`, `trusted_proxies=10.42.0.0/16`) | the HelmRelease `values.configFile` in `apps/airlift.yaml`, mounted as `$AIRLIFT_HOME/config` (a `subPath` mount) |
| admin token | the env var `AIRLIFT_ADMIN_TOKEN`, a `secretKeyRef` to key `ADMIN_PASSWORD` of the `projects-admin` Secret — the shared admin credential, owned by the projects-hub release (SOPS file `apps/secrets/projects-admin.enc.yaml`) and rendered into the `airlift` namespace |
| TLS | Traefik + cert-manager (Let's Encrypt) on the default `TLSStore`; `infrastructure/` in the infra repo |
| session data | a 50 Gi `longhorn-static` PVC (Longhorn, Delete-reclaim, thin-provisioned) holding `data_dir`: room for several streamed `airlift beam -s` uploads of up to `max_upload_bytes` (5 GiB) each, kept as sent, beside the QR beams (ADR 0024); the tower refuses an upload its free space cannot hold. Emptied on start regardless — memory-only sessions, ADR 0005 |

## Restarts

Every restart — a deploy, a config or token change, a node reboot — goes through
`Recreate`: the old pod stops before the new one starts, because the RWO volume
holds `data_dir` and only one tower may write it. Keep it that way: a rolling
overlap would mount the volume in two pods on the one node, and each tower keeps
its own sessions.

On SIGTERM the tower **drains** (docs/API.md "Stopping: the drain"): `GET /readyz`
answers 503, new sessions, upload requests, first upload parts and new event
streams get `503` + `Retry-After`, open event streams end, and the requests in
flight get up to `drain_timeout` (default 25 s) to finish before it stops. For that
to work the infra values should set:

```yaml
terminationGracePeriodSeconds: 90   # chart 0.3.0 knob; 60–120 s. Must exceed
                                    # drain_timeout + 10 s, or the kubelet kills
                                    # the drain (Kubernetes' default is 30 s)
probes:
  path: /api/info                   # liveness: a draining tower still answers it
  readinessPath: /readyz            # readiness: 503 while the tower drains
```

`drain_timeout` (restart-only) can be raised toward the grace period less 10 s to
give a part on a slow link longer to land.

## Changing configuration

Edit the HelmRelease values in `sujaykumarsuman/infra` (`apps/airlift.yaml`) and
push — Flux applies the Helm upgrade, which updates the ConfigMap. **That does not
restart the pod**: the chart carries no config checksum on the pod template, and a
`subPath` mount never sees a ConfigMap change, so the running tower keeps the file
it started with. A restart-only key (`public_url`, `listen`, `trusted_proxies`,
`drain_timeout`) takes effect on the next restart — make one after the idle check
(from the infra repo, with `AIRLIFT_ADMIN_TOKEN` loaded as its README shows):

```
hack/airlift-idle.sh && ssh sujaykumar 'k3s kubectl -n airlift rollout restart deploy/airlift'
```

Live keys (such as `max_upload_bytes`) are changed from the admin console at
runtime, need no restart, and persist in the tower's `overrides` file on the
volume. Keep `max_upload_bytes` under the volume's size: Longhorn grows the PVC in
place when `persistence.size` is raised.

## Rotating the admin token

The token is the shared `projects-admin` password. Rotate it as the infra README
describes ("The shared credential"):

```
sops apps/secrets/projects-admin.enc.yaml   # change ADMIN_PASSWORD
git commit -am "rotate the projects admin password" && git push
```

The hub re-renders the `projects-admin` Secret in every namespace within a
minute, but **airlift keeps the old token until it restarts** — the env var is read
at start. Restart it after the idle check, as above.

## Cluster details

Setup, the shared chart, secrets (SOPS + age), image automation and how to
onboard another project live in the infra repo's `README.md`. The one-off
migration from the previous Caddy + systemd host is recorded in
[`docs/build-plan/k3s-migration.md`](build-plan/k3s-migration.md).
