# Hosting airlift

The live tower runs on a single-node **k3s** cluster and is deployed by **GitOps**
— Flux + Helm reconcile it from the [`sujaykumarsuman/infra`](https://github.com/sujaykumarsuman/infra)
repo. Nothing is deployed by hand from this repo; `make` only builds.

It is served at **`https://projects.sujaykumar.dev/airlift`** (admin at
`…/airlift/admin`). Traefik terminates TLS with a Let's Encrypt certificate
(cert-manager) and strips the `/airlift` prefix before the pod, so the tower's
router stays rooted while `public_url` carries the prefix and the tower injects
`<base href="/airlift/">` (ADR 0012).

## How a change reaches production

```
git tag vX.Y.Z            # cut a release
  → .github/workflows/deploy.yml builds ghcr.io/sujaykumarsuman/airlift:X.Y.Z
      → Flux image-automation bumps the tag in sujaykumarsuman/infra
          → helm-controller upgrades the release → rollout on k3s
```

Pull-based: the cluster is never exposed to CI. There is no `make deploy` step —
tagging a release is the whole deploy.

## What runs where

| Piece | Location |
| --- | --- |
| container image | `ghcr.io/sujaykumarsuman/airlift` (built from `deploy/Dockerfile`) |
| Deployment / Service / route | rendered by the shared `project` Helm chart in `sujaykumarsuman/infra` (`apps/airlift.yaml`) |
| config (`public_url`, `listen=0.0.0.0:8443`, `trusted_proxies=10.42.0.0/16`) | the HelmRelease `values.configFile` in `apps/airlift.yaml` |
| admin token | a SOPS-encrypted Secret `airlift-admin` (`apps/secrets/airlift-admin.enc.yaml`), decrypted in-cluster by Flux |
| TLS | Traefik + cert-manager (Let's Encrypt) on the default `TLSStore`; `infrastructure/` in the infra repo |
| session data | a small `longhorn-static` PVC (Longhorn, Delete-reclaim); the session `data_dir` is emptied on start regardless — memory-only sessions, ADR 0005 |

## Changing configuration

Edit the HelmRelease values in `sujaykumarsuman/infra` (`apps/airlift.yaml`) and
push — Flux applies the Helm upgrade. Restart-only keys (`public_url`, `listen`,
`trusted_proxies`) take effect on the resulting pod roll.

## Rotating the admin token

Edit the encrypted secret in the infra repo and push:

```
sops apps/secrets/airlift-admin.enc.yaml   # change AIRLIFT_ADMIN_TOKEN
git commit -am "rotate airlift admin token" && git push
```

Flux decrypts and applies it; the pod restarts with the new token.

## Cluster details

Setup, the shared chart, secrets (SOPS + age), image automation and how to
onboard another project live in the infra repo's `README.md`. The one-off
migration from the previous Caddy + systemd host is recorded in
[`docs/build-plan/k3s-migration.md`](build-plan/k3s-migration.md).
