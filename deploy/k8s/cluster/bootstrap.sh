#!/usr/bin/env bash
# Cluster infra after k3s is up: Traefik tweaks, cert-manager, namespaces, the
# Let's Encrypt ClusterIssuers, and the shared certificate + default TLSStore for
# projects.sujaykumar.dev. Idempotent.
#
# Usage:
#   bootstrap.sh --staging   # first pass: LE staging issuer (avoids rate limits)
#   bootstrap.sh             # once the staging chain looks right: prod issuer
set -euo pipefail
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl() { k3s kubectl "$@"; }
DIR="$(cd "$(dirname "$0")" && pwd)"

ISSUER=letsencrypt-prod
[ "${1:-}" = "--staging" ] && ISSUER=letsencrypt-staging
echo "using ClusterIssuer: $ISSUER"

# 1. Traefik: preserve the client IP (externalTrafficPolicy Local) and redirect
#    web→websecure. k3s reconciles the HelmChartConfig into the Traefik release.
kubectl apply -f "$DIR/traefik-config.yaml"

# 2. cert-manager (CRDs + controllers). Check for the latest stable tag.
CM_VER="${CM_VER:-v1.16.2}"
kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CM_VER}/cert-manager.yaml"
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=180s

# 3. Namespaces, issuers, the shared cert and the default TLS store.
kubectl apply -f "$DIR/namespaces.yaml"
kubectl apply -f "$DIR/clusterissuer.yaml"
sed "s/ISSUER_PLACEHOLDER/$ISSUER/" "$DIR/certificate.yaml" | kubectl apply -f -
kubectl apply -f "$DIR/tlsstore.yaml"

echo "waiting for the certificate to be Ready (HTTP-01 needs :80 reachable)…"
if kubectl -n kube-system wait --for=condition=Ready certificate/projects-tls --timeout=180s; then
  echo "certificate Ready."
else
  echo "not Ready yet — inspect:"
  echo "  kubectl -n kube-system describe certificate projects-tls"
  echo "  kubectl -n kube-system get challenges,orders"
fi
