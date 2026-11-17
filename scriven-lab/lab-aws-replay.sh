#!/usr/bin/env bash
# Replay check for an AWS capture: prove the code can rebuild the environment
# in an account where nothing exists yet and every ID will be different.
#   1. start a second, empty Moto (moto-replay, port 5001)
#   2. terraform apply the capture to it, without its import blocks
#   3. terraform plan must then show no changes
#
# Usage: ./lab-aws-replay.sh <capture-dir>
#   <capture-dir> holds main.tf, versions.tf, ... (e.g. from `scriven capture --out`)
# Needs: docker, terraform, and the lab's Moto image (./lab-up.sh builds it).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
src="$(cd "${1:?usage: lab-aws-replay.sh <capture-dir>}" && pwd)"
export MSYS_NO_PATHCONV=1
name=moto-replay
url=http://localhost:5001
work="$(mktemp -d)"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

echo "==> 1. Empty account: $name on $url"
docker rm -f "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" -p 5001:5000 scriven-lab/moto:5.2.5 >/dev/null
for _ in $(seq 1 30); do curl -fsS "$url/moto-api/" >/dev/null 2>&1 && break; sleep 1; done

echo "==> 2. terraform apply the capture (no import blocks: nothing exists yet)"
for f in main.tf providers.tf variables.tf versions.tf .terraform.lock.hcl; do
  [[ -f "$src/$f" ]] && cp "$src/$f" "$work/"
done
# Emulator-only provider settings, kept out of the captured code.
printf 'provider "aws" {\n  s3_use_path_style = true\n}\n' > "$work/lab_override.tf"
cd "$work"
export AWS_ENDPOINT_URL="$url" HTTP_PROXY="$url" AWS_REGION=eu-west-1 AWS_DEFAULT_REGION=eu-west-1
export AWS_ACCESS_KEY_ID=replay AWS_SECRET_ACCESS_KEY=replay TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1
unset AWS_PROFILE AWS_SESSION_TOKEN
cache="$(cd "$here" && pwd -W 2>/dev/null || pwd)/.lab/aws/plugins"
[[ -d "$here/.lab/aws/plugins" ]] && export TF_PLUGIN_CACHE_DIR="$cache"
terraform init -input=false -no-color >/dev/null
terraform apply -input=false -no-color -auto-approve | grep -E '^Apply complete|^Error' || true

echo "==> 3. terraform plan against the rebuilt account"
set +e
terraform plan -input=false -no-color -detailed-exitcode > plan.txt 2>&1
rc=$?
set -e
want="$(grep -c '^resource ' main.tf)"
have="$(terraform state list | wc -l | tr -d ' ')"
if [[ $rc -eq 0 && $want -eq $have ]]; then
  echo "✓ Replay matches: $have of $want resources created, and a new plan shows no changes."
else
  echo "✗ Replay differs: $have of $want resources created, plan exit code $rc"
  tail -40 plan.txt
  exit 1
fi
