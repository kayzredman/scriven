# Scriven

Captures deployed infrastructure (Rancher on-prem, AWS, Azure, GCP) as Terraform, keeps it in a Git-backed IaC library, and watches read-only for drift.

**Status:**

- **Rancher/Kubernetes:** done. The setup wizard, the one-shot capture, and the watcher with its sweeper all work, and Scriven also runs inside a cluster as a Deployment plus a nightly CronJob.
- **AWS:** capture and sweep work for 25 resource types, from VPCs to load balancers, RDS, Lambda and KMS. Tested against the Moto emulator in the lab. The read-only check and a zero-diff capture of the first 14 types also passed in one real AWS sandbox account. Live watching is not built.
- **Azure and GCP:** not built.

All lab acceptance checks pass.

## What's here

| Path | What it is |
| --- | --- |
| `cmd/scriven/` | The CLI entry point |
| `internal/config/` | `scriven.yaml`: types, validation, `config set` |
| `internal/wizard/` | `scriven init` / `config edit`: the four-step setup wizard |
| `internal/checks/` | The live test behind each wizard step (reused by `config validate`) |
| `internal/k8s/` | Read-only Kubernetes access, including the "is this identity really read-only?" check |
| `internal/awscloud/` | Read-only AWS access: who the identity is, the proof it cannot write, and the list of resources to capture |
| `internal/capture/` | The six-step pipeline: discover, import blocks, generate HCL, clean up, verify, commit |
| `internal/library/` | Git working copy, PRs and branch protection (Gitea tested; GitHub and GitLab written but untested) |
| `internal/notify/` | Slack, Teams (Adaptive Card) and email |
| `internal/watch/` | `scriven watch` and `scriven sweep`: informers, debounce, single-resource re-capture, the observed branch, and the audit webhook |
| `internal/secrets/` | Resolves `vault://`, `env://` and `file://` references |
| `docs/` | Blueprint and whiteboard notes |
| `notes/project-memory.md` | Decisions made so far |
| `scriven-lab/` | Docker test lab and the scripts that seed it, run the replay check and deploy Scriven into it |
| `Dockerfile` | Container image: the CLI, Terraform and the pinned Kubernetes and AWS providers, with no downloads at run time |
| `deploy/` | Kustomize base for running in a cluster: watcher Deployment, sweep CronJob, audit webhook Service |
| `.github/workflows/ci.yml` | CI: format check, vet, race-enabled tests and build on every push and PR; on `main`, publishes the image to `ghcr.io/<owner>/<repo>` |

## Build

Needs Go 1.25+, Terraform 1.5+ and git on `PATH`.

```bash
go build -o bin/scriven ./cmd/scriven     # bin/scriven.exe on Windows
go test ./...
```

CI runs the same on GitHub for every push and pull request, plus a `gofmt` check and the race detector. Each push to `main` (and each `v*` tag) also builds the container image and publishes it to GitHub's container registry as `ghcr.io/<owner>/<repo>`, tagged with the short commit SHA and `latest` (or the version). The package starts private; make it public, or give your cluster a pull secret.

## Commands

| Command | Does |
| --- | --- |
| `scriven init` | Runs the wizard and writes `scriven.yaml`. Every answer is tested live before it is kept |
| `scriven config edit` | Re-runs the wizard with the current answers filled in |
| `scriven config set <key> <value>` | Changes one setting, e.g. `notify.channels "[teams, email]"`. The setting is tested first, and the old value stays if the test fails |
| `scriven config validate` | Re-tests every saved setting |
| `scriven capture [target]` | Captures the pilot (or the named target), checks for zero diff, opens a library PR and notifies |
| `scriven capture --out DIR` | Same, but writes the files to `DIR` instead of committing |
| `scriven watch` | Follows every target's watch API and records each change on the library's `observed` branch: one commit per resource, one open PR, and a notification with the diff and who made the change. `--sweep-first` runs a sweep before watching |
| `scriven sweep [target]` | Full re-capture. Any difference from the library goes to `observed`. This is the nightly safety net (run it from cron, or set `watch.sweep_at`) |

## Merge policy and library branches

`merge_policy` says, per environment, how a capture PR merges: `approvals: N` or `auto_merge: true`. Git hosts set required approvals per branch, not per environment, so an auto-merged environment cannot share `main` with one that needs approvals. Such an environment gets its own branch:

```yaml
merge_policy:
  default: {approvals: 1}
  prod:    {approvals: 2}
  dev:     {auto_merge: true, branch: dev-library}
```

The wizard sets `branch: <env>-library` by itself in that case. On first use, Scriven creates the branch empty apart from a README, so it holds only that environment's code. Capture PRs for `dev` then merge into `dev-library`. The watcher records `dev`'s changes on `observed-dev-library`, with its own PR into `dev-library`. The merge-policy check tests protection on each branch: a branch that only auto-merged environments use doesn't need protecting.

## How the watcher works

- **Feed:** a Kubernetes informer (list + watch, read-only) for every kind Scriven captures. It uses the same filter as a full capture, so system objects, controller-owned objects and Helm-managed objects are ignored.
- **Noise:** a change only counts when something a capture would write has changed. Status, `resourceVersion`, `managedFields`, the revision annotation and autoscaler-owned `replicas` don't count. Bursts are grouped by `watch.debounce` (default 15s).
- **Re-capture:** only the changed resources go through steps 2–5, including the zero-diff plan. Their blocks are then merged into the library's files in the same sorted layout a full capture writes, so a later sweep sees no phantom drift. Deletions just remove the block.
- **Library:** commits go to `observed`, one per resource, each saying what changed and who changed it. One PR from `observed` to `main` stays open. Once it's merged, `observed` starts again from `main`.
- **Who:** with `watch.audit_listen` set, each API server's audit webhook posts to `http://<scriven>/audit/<target-id>`. Notifications then name the user and the action (e.g. `alice via kubectl patch/scale`). This needs no access to nodes or log files. Without it, Scriven names the tool from `managedFields`. A change whose user agent is Terraform is labelled "expected" rather than "drift".
- **Metrics:** the `watch.audit_listen` address also serves Prometheus metrics at `/metrics`: changes recorded (drift or expected), recording errors, sweeps by result with their duration and time, audit events received and failed notifications. Alert on `scriven_last_successful_sweep_timestamp_seconds` growing old. The sweep CronJob is a separate process and reports nothing here.

Settings (`scriven config set watch.<key> <value>`): `debounce` (e.g. `10s`), `audit_listen` (e.g. `127.0.0.1:8099`), `sweep_at` (e.g. `02:00`), `aws_poll` (e.g. `60s`), `exclude_kinds` (e.g. `"[ConfigMap]"`).

`vault://` references need `VAULT_TOKEN` in the environment. The address comes from `vault.addr` in `scriven.yaml` or from `VAULT_ADDR`.

## How a Rancher capture works

1. **Discover:** connects with the read-only identity and **refuses to go on** if that identity can write anything or read Secrets. It then lists every resource kind and skips system namespaces, objects owned by controllers, built-in k3s/RKE2/Rancher objects and Helm-managed objects. Helm releases are recorded in `catalog.yaml` by chart, chart version and app version, read from the objects' labels. Scriven never reads the release Secrets that hold their values.
2. **Import blocks:** writes one `import {}` per object, into the provider's typed resources (`kubernetes_deployment_v1` and so on). Kinds with no typed resource are recorded as not captured.
3. **Generate HCL:** runs `terraform plan -generate-config-out`.
4. **Clean up:**
   - Sorts the blocks so output is stable.
   - Removes empty and zero values (some of these fail the provider's own validation) and values the cluster assigns, such as Service cluster IPs.
   - Leaves out `replicas` on a Deployment or StatefulSet that an autoscaler scales, so scaling is not reported as drift.
   - Corrects a provider misread: see the [notes](notes/project-memory.md).
5. **Verify:** `terraform plan` against the live cluster must show no changes. The only differences accepted are Terraform-only settings such as `wait_for_rollout`, and the known provider misread once it has been checked against the live object. Both are listed in `catalog.yaml`.
6. **Commit:** writes `envs/<env>/<platform>/<id>/` to a branch `capture/<env>/<id>/<time>`, opens a PR, closes older open PRs for the same target, applies the merge policy and sends notifications. If nothing changed, nothing is committed.

## How an AWS capture works

One target is one account and region. The six steps are the same as for a cluster; what differs:

1. **Discover:** logs in with the read-only identity (a profile, access keys given as secret references, or the default AWS credential chain), then assumes `role_arn` if the target sets one (with `external_id` if the role's trust policy asks for it). Scriven **refuses to go on** if the identity can write. The check is IAM policy simulation of 35 write and secret-read actions. It also simulates 7 data reads, such as `s3:GetObject` and `dynamodb:Scan`. Those are allowed with a warning in `catalog.yaml` (`data_reads`), because AWS's own `ReadOnlyAccess` grants them; a target with `metadata_only: true` refuses them instead. Where the identity may not simulate, Scriven falls back to EC2 dry-run requests, which is a weaker check, and says so in `catalog.yaml`. The default VPC, each VPC's main route table and default security group, service-linked roles, AWS-managed KMS keys, instances an Auto Scaling group launched, and CloudFormation's `cf-templates-` upload bucket are left out and listed. **Resources a CloudFormation stack manages are left out too**, as Helm-managed objects are, and `catalog.yaml` lists the stacks with a count each: the stack template is their code. IAM is account-wide, so with several targets in one account, set `global_resources: false` on all but one of them; validation catches two targets that would both capture it.
2. **Import blocks:** one per resource. Covered types: VPCs, subnets, internet gateways, route tables and their associations, security groups, Elastic IPs, NAT gateways, EC2 instances, load balancers with their listeners and target groups, S3 buckets and versioning, IAM roles with their inline and attached policies, customer-managed IAM policies, DynamoDB tables, SNS topics, RDS DB instances and subnet groups, Lambda functions, and KMS keys with their aliases. Nothing else in the account is captured yet, and `catalog.yaml` says so.
3. **Generate HCL:** as for a cluster. A resource Terraform cannot import is left out and listed, with the reason.
4. **Clean up:**
   - Removes empty and zero values, `tags_all`, the per-resource `region`, and values AWS assigns.
   - **Replaces literal IDs with references**, such as `vpc_id = aws_vpc.shop.id`. The code then works in an account where AWS hands out different IDs. A reference that would create a cycle stays a literal.
   - **Makes strings portable:** inside policies, ARNs and names, a captured resource's ARN at the start of a longer value becomes `"${aws_s3_bucket.assets.arn}/*"`, the account ID becomes `data.aws_caller_identity.current.account_id`, and the region inside an ARN becomes `var.aws_region`.
   - **Lambda code is not captured:** Scriven reads a function's configuration, never its code. Each function gets `filename = "${path.module}/lambda/<name>.zip"`, with `ignore_changes = [filename]` so the plan stays at zero diff. Put the package there before rebuilding the function elsewhere.
   - **Database passwords are not captured.** Supply one, for example in an override file, before creating a DB instance from the code.
5. **Verify:** `terraform plan` against the live account must show no changes.
6. **Commit:** as for a cluster. The library holds no credentials and no endpoint.

**Live watching for AWS:** `scriven watch` reads each AWS target's CloudTrail event history every minute (`watch.aws_poll`). Every account keeps 90 days of this history for free, and no trail is needed. Only successful write calls to the captured services count. Calls to IAM are read from `us-east-1`, where CloudTrail records them. After a burst of calls goes quiet, Scriven re-captures the target in full and checks it at zero diff. It then commits each resource whose code changed separately on the observed branch, naming the CloudTrail caller that touched it, e.g. `user/alice via console`, with the API call and its time. Calls made by Terraform are labelled as expected changes. CloudTrail takes a few minutes to show a call, so changes arrive within minutes, not seconds. The identity needs `cloudtrail:LookupEvents`. Without it, or on an emulator, the target is covered by sweeps only, and the watcher says so at start-up.


## Running in a cluster

```bash
docker build -t <registry>/scriven:<tag> .
kubectl apply -k <your-overlay>      # an overlay on top of deploy/
```

`deploy/` is a Kustomize base. Your overlay adds four things:

| What | Holds |
| --- | --- |
| ConfigMap `scriven-config` | `scriven.yaml`, with kubeconfig paths under `/etc/scriven/kubeconfigs/` and `watch.audit_listen: ":8099"` |
| Secret `scriven-kubeconfigs` | One kubeconfig per target, for Scriven's read-only identity there |
| Secret `scriven-env` | `VAULT_TOKEN`, plus anything `env://` references need |
| `images:` | The image name and tag in your registry |

What the base sets up:

- **Watcher:** a one-replica Deployment running `scriven watch --sweep-first`, so anything that changed while it was down is caught at start-up.
- **Sweeper:** a CronJob running `scriven sweep` at 02:00 UTC.
- **Audit webhook:** Service `scriven-audit` on port 8099. Point each API server's audit webhook at `/audit/<target-id>` on it.
- **Lockdown:** the pods run as non-root with a read-only root filesystem, all capabilities dropped, and the `restricted` Pod Security level. They get **no** Kubernetes API rights in the cluster they run in. Every target, including the host cluster, is reached through its read-only kubeconfig.

`scriven-lab/lab-incluster.sh` builds a complete overlay for the lab and is a working example.

## Lab quick start

```bash
cd scriven-lab
cp .env.example .env        # set the Rancher bootstrap password
./lab-up.sh                 # add "aws" (LocalStack, needs a token) or "gcp" for the optional emulators
./lab-seed.sh               # Gitea user/repo/branch protection, Vault secrets, read-only kubeconfigs
export VAULT_TOKEN=root
../bin/scriven init         # lab-seed prints the answers to give
../bin/scriven capture
./lab-replay.sh <dir>       # apply a capture (from `capture --out <dir>`) to a fresh cluster and re-capture it
../bin/scriven config set watch.audit_listen 127.0.0.1:8099   # the k3d API servers send audit events here
../bin/scriven watch        # in a second terminal
./lab-drift.sh              # 3 manual changes: expect 3 notifications and an `observed` PR
./lab-incluster.sh          # run Scriven inside prod-sim instead (--down removes it)
./lab-down.sh               # --wipe also removes data volumes
```

AWS, against the lab's Moto emulator (no AWS account, CLI or token):

```bash
./lab-aws.sh                # sample resources, a read-only and an admin identity, IAM enforcement on
../bin/scriven config edit  # add the aws target; lab-aws prints the answers to give
../bin/scriven capture shop-lab --out /tmp/shop
./lab-aws-replay.sh /tmp/shop   # rebuild it in a second, empty emulator
../bin/scriven capture shop-lab # the same, into the library as a PR; merge it
./lab-aws.sh --drift        # 3 manual changes
../bin/scriven sweep shop-lab   # expect 1 new, 1 changed, 1 removed on the `observed` PR
```

AWS, in a real sandbox account (never a production one): `scriven-lab/aws-real/shop-sample.yaml` is a CloudFormation template with the same sample resources plus a read-only `scriven-readonly` user. Upload it in the console (CloudFormation > Create stack > Upload a template file), create an access key for `scriven-readonly`, store it in Vault, and add a target with no `endpoint`:

```bash
docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=root vault \
  vault kv put secret/scriven/aws-real access_key_id=... secret_access_key=...
# target: platform aws, region of the stack,
#   access_key_id: vault://scriven/aws-real#access_key_id
#   secret_access_key: vault://scriven/aws-real#secret_access_key
```

Deleting the stack removes everything it created.

Needs Docker with Compose, k3d, kubectl and helm. Lab credentials go in `scriven-lab/.lab/`, which git ignores. Moto keeps everything in memory: after its container restarts, run `./lab-aws.sh` and capture again.

## Lab acceptance checks (Blueprint)

- [x] Wizard completes and every step's test passes against lab services
- [x] Full capture of `prod-sim` passes the zero-diff check
- [x] Replay of that capture onto a fresh k3d cluster re-captures with no differences (`lab-replay.sh`)
- [x] `lab-drift.sh` produces 3 notifications and an `observed` PR in Gitea, naming `system:admin` as the user who made each change
- [x] The tool's service account is refused when it tries to read a Secret or write anything

AWS, against Moto:

- [x] Wizard accepts an AWS target and its live test passes
- [x] Full capture of `shop-lab` (30 resources of 25 types) passes the zero-diff check
- [x] Replay of that capture into an empty emulator creates all 30 resources, and a new plan shows no changes (`lab-aws-replay.sh`, which supplies the Lambda package and a database password, as an operator would)
- [x] Resources of a CloudFormation stack are left out, and `catalog.yaml` lists the stack
- [x] After `lab-aws.sh --drift`, a sweep records 1 new, 1 changed and 1 removed resource on the `observed` PR and notifies
- [x] An identity that can write (`lab-admin`) is refused

Library branches:

- [x] With `dev: {auto_merge: true, branch: dev-library}`, a dev capture creates `dev-library` (README only), opens its PR there and merges it without approvals while `main` requires 2
- [x] A dev sweep records on `observed-dev-library`, with its PR into `dev-library`
