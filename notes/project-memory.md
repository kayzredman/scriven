# Scriven: project memory

What has been decided about this project so far. Started on 2026-11-16, last updated 2026-11-17.

## Decisions

- Building a tool to capture deployed environments that have no setup YAML or IaC records (Rancher on-prem, Google Cloud, AWS, Azure) as Terraform code, kept in a library for replay.
- It must be non-intrusive, keep running to detect changes, and send notifications.
- The open setup choices (pilot environment, library repo host, notification channel, drift-PR approval) are handled by a configurable setup wizard that runs before the tool and can be updated whenever engineers want to change details.
- Name: **Scriven** (one word; earlier candidate was "Tracewright"). Use `scriven-iac` if it is ever published, because "scriven" is already used by small unrelated projects.
- A Docker-based lab is used to test the tool (`scriven-lab/`).

## Phase 1 build (2026-11-16)

- **Engine for Kubernetes:** the provider's typed resources, through import blocks and `terraform plan -generate-config-out`. This replaces the tfk8s / `kubernetes_manifest` route in the Blueprint. After an import, `kubernetes_manifest` stores `manifest = null`, so its first plan always shows an update, and it can never pass an honest zero-diff check. Kinds with no typed resource (mostly CRDs) are listed as uncaptured in `catalog.yaml`.
- **Provider misread (hashicorp/kubernetes 3.5.0):** pod-spec fields `automountServiceAccountToken` and `enableServiceLinks` that are unset in the cluster (Kubernetes treats unset as true) are read back as `false`. Generated code that kept `false` would pass the plan yet switch both off on replay. Clean-up writes `true` when the live field is unset. The zero-diff check accepts that one difference only after confirming it against the live object, and records it in `catalog.yaml`. Re-check this when the provider is upgraded.
- **Helm releases are not captured.** Their values sit in Secrets, which the read-only identity must not read. Helm-managed objects are left out, so the library doesn't fight Helm, and are listed in `catalog.yaml`. Open question: either grant read on Helm release Secrets only (RBAC can't filter by Secret type, so this would mean all Secrets in those namespaces), or record chart and version only.
- **Read-only is enforced, not assumed.** The wizard and every capture run a SelfSubjectRulesReview and refuse an identity that can write or read Secrets.
- **Lab RBAC scope:** the lab's `scriven-readonly` ClusterRole grants built-in API groups only, so CRDs (Traefik, Gateway API and so on) are unreadable and are listed that way in the catalog. Real clusters will need a decision on which CRD groups to grant.
- **Branches:** `capture/<env>/<id>/<time>`. A new capture closes older open PRs for the same target. If the cluster still matches `main`, nothing is committed.
- **Auto-merge conflicts with repo-wide approvals:** Gitea (like GitHub) sets required approvals per branch, not per environment. With `main` needing 2 approvals for prod, `dev: auto_merge` is blocked. The wizard warns about this. Open question: give scriven's account a bypass, or keep dev captures under a separate branch or repo.
- **Generic `git` provider:** pushes branches only, with no PRs or protection checks. GitHub and GitLab API code is written but has not been run against real repos.

## Online versions

- Blueprint doc: https://claude.ai/artifact/VvWoiwkgH63uiPnBLA7zbY
- Whiteboard notes doc: https://claude.ai/artifact/U359NVpaYNbwaWWCNXx6dx
- Whiteboard: https://claude.ai/artifact/2k5PT6qfjPKLcHDGrdw9AF

The online Blueprint and Whiteboard still describe the tfk8s / manifest route for Rancher. Update them when convenient.

## Phase 3 build: watcher (2026-11-16)

- **Feed:** Kubernetes informers per captured kind, using the same `Filter.Decide` as a full capture. Changes are compared by a hash of what a capture would write, so status, `resourceVersion`, `managedFields`, the revision annotation and HPA-owned `replicas` are ignored.
- **Single-resource re-capture:** only changed objects go through import, generate, clean-up and the zero-diff plan. Their blocks are merged into the library files. Every capture path writes one canonical sorted layout (`joinBlocks`), so watcher commits and full sweeps produce byte-identical files. A sweep straight after the watcher has run reports "up to date".
- **Library flow:** the `observed` branch gets one commit per resource, and one PR `observed` → `main` stays open. Once everything on `observed` is merged, it restarts from `main`. If it can't merge `main` cleanly, it resets to `main` with a force push, and the next sweep re-detects anything still different.
- **Who changed it:** the API server audit webhook pushes to `scriven watch` at `/audit/<target-id>`. This is push-based with no node access, and if Scriven is down the API server just drops the events. The audit event supplies the user, tool and action. `managedFields` is only a fallback, because the API server leaves `scale` writes untimed and the latest timestamped manager is then wrong.
- **Expected vs drift:** "expected" when the writer's user agent is Terraform, otherwise "drift". There is no record of planned Terraform runs yet.
- **Lab:** the k3d clusters are created with the audit policy and webhook config from a named Docker volume (`scriven-audit`). Bind mounts break on Windows paths with drive letters and spaces. Webhook target: `host.k3d.internal:8099`.
- **Not done yet:**
  - Container image plus Deployment/CronJob manifests for running in-cluster.
  - Per-environment notification routing.

## In-cluster deployment (2026-11-17)

- **Image (`Dockerfile`):** multi-stage. Terraform is checked against HashiCorp's SHA256SUMS, and the Kubernetes provider is pre-fetched. The provider is served from a **filesystem mirror** (`TF_CLI_CONFIG_FILE`), not a plugin cache: with no lock file, Terraform rewrites cache entries, which fails on a read-only root filesystem. Captures need no network access to the registry.
- **Manifests (`deploy/`):** a Kustomize base. Watcher Deployment (1 replica, `Recreate`, `--sweep-first`), sweep CronJob (02:00 UTC), audit Service, NetworkPolicy (ingress on 8099 only), namespace at the `restricted` Pod Security level.
- **No in-cluster API rights:** the pods' ServiceAccount has no token mounted. Even the cluster Scriven runs in is reached through a read-only kubeconfig, so the "refuse an identity that can write" check applies everywhere.
- **Change detection ignores bookkeeping files:** `catalog.yaml` (timestamp) and `.terraform.lock.hcl` (provider hashes differ between Windows and Linux). Otherwise a Linux sweep after a Windows capture committed and notified with nothing actually changed.
- **Namespace `scriven-system`** is excluded from capture, like `scriven`.
- **Lab:** `lab-incluster.sh` deploys into `prod-sim` and watches both clusters. Pods can't resolve Docker container names, so the overlay pins them with `hostAliases`. `lab-audit-target.sh host|cluster` switches where the API servers send audit events. It restarts each k3d server and then its load balancer, because a restarted server can get a new IP and the load balancer keeps forwarding to the old one (which may now belong to the other cluster).
- **Speed:** in-cluster on Linux, a change is recorded in about 6–10 seconds after the debounce. On the Windows host it took about 35 seconds.
- **Not done yet:** image publishing to a registry or CI, and an egress NetworkPolicy (addresses are site-specific).

## Phase 2 build: AWS capture (2026-11-17)

- **No AWS sign-up:** the user did not want an AWS or LocalStack account. AWS is developed against **Moto** (open source, no token) in the lab. Nothing here has run against a real AWS account, so treat real-AWS behaviour as unproven.
- **Target shape:** one target is one account and region. Settings: `region`, then either `profile`, or `access_key_id` + `secret_access_key` as secret references, or neither (default credential chain). `endpoint` is for emulators only.
- **Same engine as Kubernetes:** AWS SDK lists resources, then import blocks and `terraform plan -generate-config-out`, clean-up, and the zero-diff plan. Version 0.3.0.
- **Covered types (14):** VPC, subnet, internet gateway, route table and association, security group, S3 bucket and versioning, IAM role, inline role policy, customer-managed policy, role-policy attachment, DynamoDB table, SNS topic. Everything else is uncaptured and `catalog.yaml` says so.
- **Left out on purpose:** the default VPC, each VPC's main route table and default security group, and service-linked roles. AWS creates them and Terraform can only adopt them. Risk: routes someone added to a main route table are not captured.
- **References instead of IDs:** clean-up replaces a captured resource's ID, where another resource uses it, with a reference (`aws_vpc.shop.id`). Opaque IDs and ARNs are replaced anywhere. Names (role name, bucket name) are replaced only in the attribute that means them (`role`, `bucket`). A reference that would close a cycle stays a literal. This is what makes replay into another account work.
- **Read-only check:** first choice is `iam:SimulatePrincipalPolicy` over 26 write and secret-read actions. If that call is not available, the fallback is EC2 dry-run requests, recorded in `catalog.yaml` as the weaker check. The dry-run probes only ever "delete" IDs that do not exist: Moto ignores `DryRun` on some calls, and an earlier probe (`CreateVpc`) really created a VPC in the emulator.
- **Open question:** `s3:GetObject` is not on the refuse list, because AWS's own `ReadOnlyAccess` policy grants it. Decide whether Scriven should insist on a metadata-only identity (`ViewOnlyAccess` style).
- **SQS dropped for now:** the provider cannot import a queue whose URL is not an `amazonaws.com` URL, so it cannot be tested on an emulator.
- **Emulator plumbing, kept out of the library:** with `endpoint` set, the scratch directory gets an override file (`s3_use_path_style`), and Terraform runs with `HTTP_PROXY` pointing at the emulator. The provider calls S3 Control at `<account-id>.<host>`, which does not resolve locally; sent through the emulator as a proxy, the request arrives.
- **Lab Moto is patched:** `scriven-lab/moto/Dockerfile` pins 5.2.5 and makes S3 Control use Moto's S3 signer. Without it, with IAM enforcement on, reading bucket tags fails for every identity and no bucket can be imported.
- **IAM is global:** an account with targets in several regions would capture its roles and policies in each. Not handled yet.
- **Sweep only:** AWS targets are not watched live. `scriven watch` logs that and sweeps them (`--sweep-first`, `watch.sweep_at`). Drift notifications cannot name who made the change. CloudTrail or EventBridge is the likely feed.
- **Not done yet:**
  - A run against a real AWS account.
  - Live watching.
  - More resource types (EC2 instances, RDS, Lambda, load balancers, NAT gateways, KMS, and so on).
  - Assume-role settings in `scriven.yaml`.
  - Generated code keeps every `false` default, so it is wordier than a person would write.

## Hardening sprint (2026-11-17)

- **Slow captures were mostly a re-download.** Every capture starts in an empty directory with no lock file, and Terraform then ignores its plugin cache and downloads the provider again (800 MB for AWS). Fix: the first full lock file Terraform writes for a provider (one with `zh:` checksums for every platform) is saved beside the cache in `locks/`, and later captures start from it. `terraform init` went from about 2 minutes to 30 seconds, with the same lock file and `main.tf` as before. `TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE` was tried and rejected: it writes a lock with only this platform's checksum, which would break `terraform init` on any other OS.
- **Provider pinning:** a saved lock pins that provider version on this machine. Delete `<user cache>/scriven/terraform-plugins/locks/<provider>.lock.hcl` to move to a newer provider.
- **What is still slow on the Windows host:** each Terraform command starts the 800 MB AWS provider, and Windows Defender scans it (the first start took 67 seconds, later ones about 10). An AWS capture still takes about 3.5 minutes there. Not changed: a Defender exclusion is a decision for the machine's owner.
- **Image:** pre-fetches `hashicorp/aws` as well as `hashicorp/kubernetes`, both served from the read-only filesystem mirror. `SCRIVEN_PLUGIN_CACHE=off` in the image stops scriven adding a plugin cache, which would copy the providers into `/tmp`.
- **Autoscaled workloads:** a capture leaves out `spec.replicas` on a Deployment or StatefulSet that an HPA scales (the provider marks it optional and computed, so the plan stays at zero diff). A full capture works this out from the HPAs it discovers; the watcher uses its HPA informer. Gap: adding or removing an HPA does not re-capture its target until the next sweep.
- **catalog.yaml on watcher commits:** each watcher commit updates `resource_count` and sets `observed_at`. The rest (uncaptured objects, provider versions) stays as the last full capture or sweep wrote it.
- **Metrics:** `scriven watch` serves Prometheus text at `/metrics` on the `watch.audit_listen` address, written by hand (no client library). Counters: changes recorded by label, record errors, sweeps by result, audit events, failed notifications. Gauges: up, targets by mode, per-target watch state, last sweep duration and time, last successful sweep time, resources recorded by the last sweep. The sweep CronJob is a separate process and exports nothing.

## First run against real AWS (2026-11-17)

- **Setup:** a sandbox account, provisioned by the user from the console with the CloudFormation template `scriven-lab/aws-real/shop-sample.yaml` (no AWS CLI). It holds the same sample resources as the Moto lab and a metadata-only `scriven-readonly` user that may also call `iam:SimulatePrincipalPolicy`. The access key goes into the lab Vault at `secret/scriven/aws-real`, typed by the user. Keys are never pasted into chat.
- **Result:** IAM policy simulation proved the identity read-only (the full check, which Moto cannot do), and the capture passed the zero-diff plan. AWS was reachable from this machine with no proxy.
- **Found on real AWS, fixed:**
  - CloudFormation's `cf-templates-<random>-<region>` upload bucket was captured. It is now left out and listed.
  - The account ID survived inside strings: policy resources (`arn:aws:s3:::bucket/*`), the SNS default policy, bucket names. Clean-up now writes a captured resource's ARN at the start of a string as `${aws_x.y.arn}`, the account as `data.aws_caller_identity.current.account_id` (`providers.tf` then declares the data source), and the region inside ARNs as `var.aws_region`. Moto never showed this: its replay account has the same fake ID.
- **Open question:** resources a CloudFormation stack manages are captured like any others, so the library and CloudFormation would both claim them, as with Helm. Decide whether to skip them (the `aws:cloudformation:stack-name` tag identifies them).
- **Not yet tested on real AWS:** replay into a second, empty account.
