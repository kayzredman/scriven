#!/usr/bin/env bash
# Bring up the scriven test lab: support services + two k3d clusters + sample workloads.
# Needs: docker (with compose), k3d, kubectl, helm.
set -euo pipefail
cd "$(dirname "$0")"

PROFILES=("$@")   # e.g. ./lab-up.sh aws gcp
PROFILE_ARGS=()
for p in "${PROFILES[@]:-}"; do [[ -n "$p" ]] && PROFILE_ARGS+=(--profile "$p"); done

for bin in docker k3d kubectl helm; do
  command -v "$bin" >/dev/null || { echo "missing: $bin"; exit 1; }
done
[[ -f .env ]] || { echo "copy .env.example to .env first"; exit 1; }

echo "==> Starting support services"
docker compose "${PROFILE_ARGS[@]}" up -d

echo "==> Creating k3d clusters on the shared network"
for c in prod-sim dev-sim; do
  if ! k3d cluster list "$c" >/dev/null 2>&1; then
    k3d cluster create "$c" --network scriven-lab --agents 1 --wait
  fi
done

echo "==> Seeding sample workloads (things for the tool to capture)"
for c in prod-sim dev-sim; do
  ctx="k3d-$c"
  kubectl --context "$ctx" create namespace shop --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
  kubectl --context "$ctx" -n shop apply -f sample/workloads.yaml
  helm --kube-context "$ctx" upgrade --install api oci://ghcr.io/stefanprodan/charts/podinfo \
    -n shop --set replicaCount=1 --wait || \
    echo "   (helm chart skipped: registry not reachable)"
done

echo "==> Read-only identity for the capture tool"
for c in prod-sim dev-sim; do
  kubectl --context "k3d-$c" apply -f sample/readonly-rbac.yaml
done

cat <<EOF

Lab is up.
  Rancher   https://localhost:8443   (import k3d-prod-sim and k3d-dev-sim; server URL: https://rancher)
  Gitea     http://localhost:3000    (create repo: iac-library)
  Vault     http://localhost:8200    (token from .env)
  Mailpit   http://localhost:8025
  Webhooks  docker logs -f webhook-catcher
Next: ./lab-drift.sh to simulate a manual change for the watcher to catch.
EOF
