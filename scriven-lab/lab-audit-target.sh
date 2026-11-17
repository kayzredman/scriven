#!/usr/bin/env bash
# Point both lab API servers' audit webhooks at scriven:
#   host     scriven watch running on this machine (port 8099)
#   cluster  scriven running in prod-sim (lab-incluster.sh), via NodePort 30099
# The API server reads its webhook config at start-up, so each server node is restarted.
set -euo pipefail
cd "$(dirname "$0")"
export MSYS_NO_PATHCONV=1

case "${1:-}" in
  host)    url="http://host.k3d.internal:8099" ;;
  cluster) url="http://k3d-prod-sim-server-0:30099" ;;
  *) echo "usage: $0 host|cluster"; exit 1 ;;
esac

for c in prod-sim dev-sim; do
  sed -e "s/__TARGET__/$c/" -e "s#http://host.k3d.internal:8099#$url#" \
    audit/webhook.kubeconfig.tmpl > "audit/.webhook-$c.kubeconfig"
done
docker run --rm -v scriven-audit:/cfg -v "$(pwd)/audit:/src:ro" busybox \
  sh -c "cp /src/policy.yaml /src/.webhook-*.kubeconfig /cfg/"

for c in prod-sim dev-sim; do
  docker restart "k3d-$c-server-0" >/dev/null
done
# A restarted container can get a new IP, and each k3d load balancer (nginx)
# resolved its server's IP at start-up: restart them too, or they forward to
# whichever container now has the old address.
for c in prod-sim dev-sim; do
  docker restart "k3d-$c-serverlb" >/dev/null
done
for c in prod-sim dev-sim; do
  ok=""
  for _ in $(seq 1 60); do
    if kubectl --context "k3d-$c" get --raw /readyz >/dev/null 2>&1; then ok=1; break; fi
    sleep 2
  done
  [[ -n $ok ]] || { echo "   $c: API server did not come back"; exit 1; }
  echo "   $c: audit events -> $url/audit/$c"
done
