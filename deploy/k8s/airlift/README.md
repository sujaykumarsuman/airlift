# airlift on k3s (the `airlift` namespace)

Single-replica deployment of the tower. Full rationale and the distributed-
architecture plan: [`docs/build-plan/k3s-migration.md`](../../../docs/build-plan/k3s-migration.md).
The shared cluster (k3s, Traefik, cert-manager, the TLS cert, namespaces) is set
up first from [`../cluster/`](../cluster/).

## Deploy

Prerequisites: the cluster is up (`../cluster/`), the `airlift` namespace exists,
and the image is built + imported into k3s' containerd (no registry):

```
# on the VPS, from a clean checkout that includes the trusted_proxies-CIDR change
docker build -f deploy/Dockerfile --build-arg VERSION=$(git describe --tags) \
  -t airlift:$(git describe --tags) -t airlift:latest .
docker save airlift:$(git describe --tags) airlift:latest | k3s ctr images import -
```

Create the admin Secret from the token already live on the VPS (so it is not
rotated), then apply:

```
TOKEN=$(sed -n 's/^AIRLIFT_ADMIN_TOKEN=//p' /etc/default/airlift)
kubectl -n airlift create secret generic airlift-admin \
  --from-literal=AIRLIFT_ADMIN_TOKEN="$TOKEN"

kubectl apply -k deploy/k8s/airlift
kubectl -n airlift rollout status deploy/airlift
```

`make k8s-deploy` (repo root) wraps the build + import + apply + rollout.

## Redeploy

The Deployment pins `airlift:latest` with `imagePullPolicy: IfNotPresent`.
Rebuild + re-import (overwrites `airlift:latest` in containerd), then:

```
kubectl -n airlift rollout restart deploy/airlift
```

The binary is stamped with `VERSION` (git describe), so `GET /api/info` reports
the true version regardless of the image tag.

## Verify

```
curl -s https://projects.sujaykumar.dev/airlift/api/info | jq .version
kubectl -n airlift logs deploy/airlift | head   # a real client IP, not 10.42.x.x
```

## Notes

- **Secret**: `secret.example.yaml` is an example and is not applied by kustomize.
- **Config**: `configmap.yaml` sets `listen=0.0.0.0:8443` and
  `trusted_proxies=10.42.0.0/16` (the k3s pod CIDR). These are restart-only.
- **TLS**: the route uses the default TLSStore cert (no per-namespace secret).
- **Storage**: `local-path` RWO PVC; only `overrides`/`config` need to survive a
  restart (sessions are memory-only).
