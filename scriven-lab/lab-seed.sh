#!/usr/bin/env bash
# Prepare the lab for scriven after ./lab-up.sh: everything the wizard will test against.
#   - Gitea: admin "scriven" with an API token, two reviewers, repo iac-library,
#            main protected (PR-only, 2 approvals)
#   - Vault: secret/scriven/git-token and secret/scriven/teams-webhook
#   - Kubeconfigs for the scriven-readonly service account, one per k3d cluster
# Safe to re-run. Lab credentials go to .lab/ (git-ignored).
set -euo pipefail
cd "$(dirname "$0")"
export MSYS_NO_PATHCONV=1   # Git Bash on Windows: don't rewrite /paths in docker exec args

source .env
GITEA=http://localhost:3000
VAULT_TOKEN="${VAULT_DEV_TOKEN:-root}"
mkdir -p .lab

rand() { head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 20; }

echo "==> Gitea users"
pw_file=.lab/gitea-users.txt
[[ -f $pw_file ]] || : > "$pw_file"
for user in scriven reviewer1 reviewer2; do
  if ! grep -q "^$user " "$pw_file"; then
    pw="$(rand)"
    admin=""; [[ $user == scriven ]] && admin="--admin"
    docker exec -u git gitea gitea admin user create --username "$user" --password "$pw" \
      --email "$user@lab.local" --must-change-password=false $admin >/dev/null
    echo "$user $pw" >> "$pw_file"
    echo "   created $user"
  fi
done
admin_pw="$(awk '$1=="scriven"{print $2}' "$pw_file")"

if [[ ! -s .lab/gitea-token ]]; then
  docker exec -u git gitea gitea admin user generate-access-token --username scriven \
    --token-name "scriven-$(date +%s)" --scopes all --raw | tr -d '\r\n' > .lab/gitea-token
fi
TOKEN="$(cat .lab/gitea-token)"
api() { curl -fsS -H "Authorization: token $TOKEN" -H "Content-Type: application/json" "$@"; }

echo "==> Library repo scriven/iac-library"
if ! api "$GITEA/api/v1/repos/scriven/iac-library" >/dev/null 2>&1; then
  api -X POST "$GITEA/api/v1/user/repos" -d '{"name":"iac-library","private":true,"auto_init":true,"default_branch":"main","readme":"Default","description":"Scriven IaC library (lab)"}' >/dev/null
fi
for r in reviewer1 reviewer2; do
  api -X PUT "$GITEA/api/v1/repos/scriven/iac-library/collaborators/$r" -d '{"permission":"write"}' >/dev/null
done

echo "==> Protect main: PR-only, 2 approvals"
if ! api "$GITEA/api/v1/repos/scriven/iac-library/branch_protections/main" >/dev/null 2>&1; then
  api -X POST "$GITEA/api/v1/repos/scriven/iac-library/branch_protections" \
    -d '{"rule_name":"main","enable_push":false,"required_approvals":2,"enable_approvals_whitelist":false,"block_on_rejected_reviews":true}' >/dev/null
fi

echo "==> Vault secrets (KV v2 at secret/)"
vault() { docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN="$VAULT_TOKEN" vault vault "$@"; }
vault kv put secret/scriven/git-token value="$TOKEN" >/dev/null
# The webhook catcher stands in for a Teams/Slack incoming webhook.
vault kv put secret/scriven/teams-webhook value="http://localhost:8081/teams" >/dev/null
vault kv put secret/scriven/slack-webhook value="http://localhost:8081/slack" >/dev/null

echo "==> Kubeconfigs for the read-only service account"
for c in prod-sim dev-sim; do
  ctx="k3d-$c"
  server="$(kubectl config view -o jsonpath="{.clusters[?(@.name==\"$ctx\")].cluster.server}" | sed 's#0\.0\.0\.0#127.0.0.1#')"
  kubectl config view --raw -o jsonpath="{.clusters[?(@.name==\"$ctx\")].cluster.certificate-authority-data}" \
    | base64 -d > ".lab/$c-ca.crt"
  token="$(kubectl --context "$ctx" -n scriven create token scriven-readonly --duration=720h)"
  kc=".lab/$c.kubeconfig"
  rm -f "$kc"
  kubectl config --kubeconfig "$kc" set-cluster "$c" --server "$server" \
    --certificate-authority ".lab/$c-ca.crt" --embed-certs=true >/dev/null
  kubectl config --kubeconfig "$kc" set-credentials scriven-readonly --token "$token" >/dev/null
  kubectl config --kubeconfig "$kc" set-context "scriven@$c" --cluster "$c" --user scriven-readonly >/dev/null
  kubectl config --kubeconfig "$kc" use-context "scriven@$c" >/dev/null
  rm -f ".lab/$c-ca.crt"
  echo "   $kc  (token valid 30 days)"
done

cat <<EOF

Lab seeded. Answers for \`scriven init\`:
  Vault address        http://localhost:8200   (export VAULT_TOKEN=$VAULT_TOKEN)
  Target               rancher / prod-sim / prod, kubeconfig $(pwd)/.lab/prod-sim.kubeconfig
  Library              gitea, $GITEA/scriven/iac-library.git, user scriven, vault://scriven/git-token
  Teams webhook        vault://scriven/teams-webhook
  Email                SMTP localhost:1025, from scriven@lab.local
Gitea logins (admin + 2 reviewers) are in .lab/gitea-users.txt
EOF
