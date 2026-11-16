#!/usr/bin/env bash
# Replay check: prove a capture can rebuild its environment.
#   1. create an empty k3d cluster (replay-sim)
#   2. terraform apply the capture to it, as the cluster admin
#   3. capture replay-sim with scriven's read-only identity
#   4. the two captures must match, apart from cluster-specific values
#
# Usage: ./lab-replay.sh <capture-dir> [scriven.yaml]
#   <capture-dir> holds main.tf, versions.tf, ... (e.g. from `scriven capture --out`)
# Needs: k3d, kubectl, terraform, and bin/scriven(.exe) built.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
src="$(cd "${1:?usage: lab-replay.sh <capture-dir> [scriven.yaml]}" && pwd)"
cfg="${2:-$here/../scriven.yaml}"
scriven="$here/../bin/scriven"; [[ -x $scriven ]] || scriven="$here/../bin/scriven.exe"
cluster=replay-sim
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
winpath() { if command -v cygpath >/dev/null; then cygpath -m "$1"; else echo "$1"; fi; }

echo "==> 1. Empty cluster $cluster"
k3d cluster delete "$cluster" >/dev/null 2>&1 || true
k3d cluster create "$cluster" --network scriven-lab --agents 0 --wait >/dev/null
admin_kc="$work/admin.kubeconfig"
k3d kubeconfig get "$cluster" | sed 's#0\.0\.0\.0#127.0.0.1#' > "$admin_kc"

echo "==> 2. terraform apply the capture (no import blocks: the objects don't exist yet)"
mkdir -p "$work/tf"
for f in main.tf providers.tf variables.tf versions.tf .terraform.lock.hcl; do
  [[ -f "$src/$f" ]] && cp "$src/$f" "$work/tf/"
done
(
  cd "$work/tf"
  export TF_VAR_kubeconfig_path="$(winpath "$admin_kc")" TF_VAR_kube_context="k3d-$cluster" TF_IN_AUTOMATION=1
  terraform init -input=false -no-color >/dev/null
  terraform apply -input=false -no-color -auto-approve | grep -E '^Apply complete|Error' || true
)

echo "==> 3. Read-only identity on $cluster, then capture it"
kubectl --kubeconfig "$admin_kc" apply -f "$here/sample/readonly-rbac.yaml" >/dev/null
ro_kc="$work/ro.kubeconfig"
server="$(kubectl --kubeconfig "$admin_kc" config view -o jsonpath='{.clusters[0].cluster.server}')"
kubectl --kubeconfig "$admin_kc" config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$work/ca.crt"
token="$(kubectl --kubeconfig "$admin_kc" -n scriven create token scriven-readonly --duration=1h)"
kubectl config --kubeconfig "$ro_kc" set-cluster "$cluster" --server "$server" --certificate-authority "$work/ca.crt" --embed-certs=true >/dev/null
kubectl config --kubeconfig "$ro_kc" set-credentials scriven-readonly --token "$token" >/dev/null
kubectl config --kubeconfig "$ro_kc" set-context "scriven@$cluster" --cluster "$cluster" --user scriven-readonly >/dev/null
kubectl config --kubeconfig "$ro_kc" use-context "scriven@$cluster" >/dev/null

# Same settings, but the pilot target points at the replay cluster.
sed -e "0,/kubeconfig: .*/s##kubeconfig: $(winpath "$ro_kc")#" "$cfg" > "$work/scriven.yaml"
"$scriven" -c "$work/scriven.yaml" capture -q --out "$work/recapture"

echo "==> 4. Compare"
# Context names differ by design (kube_context default); everything else must match.
norm() { sed -E 's/(default *= *)"scriven@[a-z-]+"/\1"<context>"/' "$1"; }
status=0
for f in main.tf imports.tf; do
  if diff <(norm "$src/$f") <(norm "$work/recapture/$f") > "$work/$f.diff"; then
    echo "   $f: identical"
  else
    echo "   $f: DIFFERENT"; sed 's/^/     /' "$work/$f.diff" | head -40; status=1
  fi
done
grep -E 'resource_count|zero_diff:' "$work/recapture/catalog.yaml" | sed 's/^/   re-capture /'

k3d cluster delete "$cluster" >/dev/null
if [[ $status -eq 0 ]]; then
  echo "Replay check passed: the capture rebuilds an environment that captures back identically."
else
  echo "Replay check FAILED: see differences above."
fi
exit $status
