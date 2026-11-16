#!/usr/bin/env bash
# Simulate "someone changed prod by hand" so the watcher has something to detect.
set -euo pipefail
ctx="k3d-prod-sim"

echo "==> Drift 1: scale the web deployment"
kubectl --context "$ctx" -n shop scale deployment/web --replicas=4

echo "==> Drift 2: change a ConfigMap value"
kubectl --context "$ctx" -n shop patch configmap web-config --type merge \
  -p '{"data":{"FEATURE_FLAG":"on"}}'

echo "==> Drift 3: add an unmanaged service"
kubectl --context "$ctx" -n shop create service clusterip rogue --tcp=9000:9000 \
  --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -

echo "Done. Expect 3 drift notifications (webhook-catcher logs / Mailpit) and an 'observed' PR in Gitea."
