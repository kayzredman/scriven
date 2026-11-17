#!/usr/bin/env bash
# Seed the lab's AWS stand-in (Moto, http://localhost:5000) for scriven:
#   - sample resources (sample-aws/resources): a VPC, subnets, security groups,
#     a bucket, an IAM role and policies, a DynamoDB table, an SNS topic
#   - identities (sample-aws/identities): scriven-readonly and lab-admin, with
#     their access keys stored in Vault
#   - then switches Moto's IAM enforcement on, so a write by the read-only
#     identity is really refused
# Safe to re-run. Moto keeps everything in memory: after its container
# restarts, run this again and re-capture (the resources get new IDs).
#   ./lab-aws.sh           seed, repair, or undo --drift
#   ./lab-aws.sh --reset   wipe Moto first
#   ./lab-aws.sh --drift   change the account "by hand": open SSH on the web
#                          security group, add a bucket, delete the SNS topic
# Needs: docker, terraform. No AWS account, CLI or token.
set -euo pipefail
cd "$(dirname "$0")"
export MSYS_NO_PATHCONV=1

mode="${1:-}"
case "$mode" in
  ""|--reset|--drift) ;;
  *) echo "usage: $0 [--reset|--drift]"; exit 1 ;;
esac

source .env
MOTO=http://localhost:5000
VAULT_TOKEN="${VAULT_DEV_TOKEN:-root}"
# pwd -W: a Windows path under Git Bash, which the Windows terraform.exe needs
STATE="$(pwd -W 2>/dev/null || pwd)/.lab/aws"
mkdir -p "$STATE/resources" "$STATE/identities"

command -v terraform >/dev/null || { echo "missing: terraform"; exit 1; }
curl -fsS "$MOTO/moto-api/" >/dev/null 2>&1 || { echo "Moto is not answering on $MOTO: run ./lab-up.sh"; exit 1; }

if [[ $mode == --reset ]]; then
  echo "==> Wiping Moto"
  curl -fsS -X POST "$MOTO/moto-api/reset" >/dev/null
  rm -f "$STATE"/*/terraform.tfstate*
fi

# Seeding has to write, so enforcement is lifted while it runs.
# (text/plain: Moto reads the raw body, which a form post would leave empty)
auth() { curl -fsS -X POST -H "Content-Type: text/plain" --data-binary "$1" "$MOTO/moto-api/reset-auth" >/dev/null; }
auth 1000000
trap 'auth 0' EXIT

export AWS_ENDPOINT_URL="$MOTO"
# The provider calls S3 Control at <account-id>.localhost, a name that does not
# resolve. Sent through Moto as an HTTP proxy, those requests arrive anyway.
export HTTP_PROXY="$MOTO"
export AWS_REGION=eu-west-1 AWS_DEFAULT_REGION=eu-west-1
export AWS_ACCESS_KEY_ID=seed AWS_SECRET_ACCESS_KEY=seed
unset AWS_PROFILE AWS_SESSION_TOKEN
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1
# One provider download shared by both configs (the AWS provider is large).
export TF_PLUGIN_CACHE_DIR="$STATE/plugins"
mkdir -p "$TF_PLUGIN_CACHE_DIR"

apply() {  # apply <resources|identities>
  terraform -chdir="sample-aws/$1" init -input=false -no-color >/dev/null
  terraform -chdir="sample-aws/$1" apply -input=false -no-color -auto-approve \
    -state="$STATE/$1/terraform.tfstate" | grep -E '^(Apply complete|No changes)' | sed 's/^/   /'
}
out() { terraform -chdir=sample-aws/identities output -state="$STATE/identities/terraform.tfstate" -raw "$1"; }

if [[ $mode == --drift ]]; then
  echo "==> Drift: open SSH on shop-web, add bucket shop-exports-lab, delete topic shop-alerts"
  TF_VAR_drift=true apply resources
  auth 0
  trap - EXIT
  echo "Done. \`scriven sweep shop-lab\` should record 1 changed, 1 new and 1 removed resource."
  exit 0
fi

echo "==> Sample resources"
apply resources
echo "==> Identities: scriven-readonly, lab-admin"
apply identities

echo "==> Access keys into Vault"
vault() { docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN="$VAULT_TOKEN" vault vault "$@"; }
vault kv put secret/scriven/aws-readonly \
  access_key_id="$(out readonly_access_key_id)" secret_access_key="$(out readonly_secret_access_key)" >/dev/null
vault kv put secret/scriven/aws-admin \
  access_key_id="$(out admin_access_key_id)" secret_access_key="$(out admin_secret_access_key)" >/dev/null

echo "==> IAM enforcement on"
auth 0
trap - EXIT

cat <<MSG

AWS stand-in seeded. Answers for \`scriven init\` (add a target):
  Platform / ID / env   aws / shop-lab / dev
  Region                eu-west-1
  Access key ID         vault://scriven/aws-readonly#access_key_id
  Secret access key     vault://scriven/aws-readonly#secret_access_key
  Endpoint              $MOTO
To see scriven refuse an identity that can write, use
vault://scriven/aws-admin#access_key_id (and #secret_access_key) instead.
MSG
