# Shared k3s cluster infra

Set up **once** on the VPS; reused by every project namespace. Rationale and the
full cutover runbook: [`docs/build-plan/k3s-migration.md`](../../../docs/build-plan/k3s-migration.md).

This is the "make the VPS host further projects" layer: a project = a namespace
+ its workload + an IngressRoute for its path prefix under
`projects.sujaykumar.dev`. TLS is shared (one cert, the default TLSStore), so a
new project needs no certificate of its own.

## Order (part of the watched cutover — Caddy must be stopped first)

```
bash install-k3s.sh              # installs k3s; Traefik/ServiceLB take :80/:443
bash bootstrap.sh --staging      # cert-manager, issuers, cert (LE staging), TLSStore
#   confirm the staging chain, then:
bash bootstrap.sh                # re-issues with the LE prod issuer
```

Then deploy the projects: airlift (`../airlift/`) and the hub (in the
`sujaykumarsuman.github.io` repo, `projects/deploy/k8s/`).

## What each file is

| File | Purpose |
| --- | --- |
| `install-k3s.sh` | installs k3s (single server), bundled Traefik kept |
| `traefik-config.yaml` | `HelmChartConfig`: client-IP preservation + web→websecure redirect |
| `namespaces.yaml` | one namespace per project (`airlift`, `projects-hub`, …) |
| `clusterissuer.yaml` | Let's Encrypt staging + prod, HTTP-01 via Traefik |
| `certificate.yaml` | the one shared cert for the host (in `kube-system`) |
| `tlsstore.yaml` | Traefik default TLSStore adopting that cert |
| `bootstrap.sh` | applies all of the above, waits for the cert |

## Adding a future project

1. Add its namespace to `namespaces.yaml` (and re-apply).
2. Deploy its workload + Service in that namespace.
3. Add a Traefik `IngressRoute` (host `projects.sujaykumar.dev`,
   `PathPrefix(\`/<project>\`)`, a StripPrefix middleware, `tls: {}`,
   `priority` above the hub's catch-all). Copy `../airlift/` as the template.
4. Link it from the hub page.

## Verify / debug

```
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl get nodes
kubectl -n kube-system get certificate,tlsstore
kubectl -n kube-system describe certificate projects-tls
kubectl api-resources | grep traefik   # confirm the traefik.io/v1alpha1 CRDs
```
