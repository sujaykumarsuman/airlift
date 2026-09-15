#!/usr/bin/env bash
# Install k3s (single server node) on the VPS. Keeps the bundled Traefik and
# ServiceLB (klipper), which bind :80/:443 on the node — so Caddy must already be
# stopped (see the cutover runbook, docs/build-plan/k3s-migration.md §9).
# Idempotent: re-running the installer is a no-op if k3s is current.
set -euo pipefail

if command -v k3s >/dev/null 2>&1; then
  echo "k3s already installed: $(k3s --version | head -1)"
else
  # Pin a channel/version for reproducibility; --write-kubeconfig-mode 0644 lets
  # kubectl read the config without sudo juggling.
  curl -sfL https://get.k3s.io | \
    INSTALL_K3S_EXEC="server --write-kubeconfig-mode 0644" sh -
fi

echo "waiting for the node to be Ready…"
until k3s kubectl get nodes 2>/dev/null | grep -q ' Ready '; do sleep 3; done
k3s kubectl get nodes
echo
echo "kubeconfig: /etc/rancher/k3s/k3s.yaml"
echo "next: bash $(dirname "$0")/bootstrap.sh --staging   # then re-run without --staging for prod TLS"
