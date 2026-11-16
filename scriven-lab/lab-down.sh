#!/usr/bin/env bash
# Tear the lab down. Pass --wipe to also delete Rancher and Gitea data volumes.
set -euo pipefail
cd "$(dirname "$0")"

for c in prod-sim dev-sim; do k3d cluster delete "$c" 2>/dev/null || true; done

if [[ "${1:-}" == "--wipe" ]]; then
  docker compose --profile aws --profile gcp down -v
  docker volume rm scriven-audit >/dev/null 2>&1 || true
else
  docker compose --profile aws --profile gcp down
fi
