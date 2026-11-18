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
- **Helm releases are not captured** (decided 2026-11-17). Their values sit in release Secrets, and RBAC cannot grant read on those without every other Secret in the namespace, so Scriven never reads them. Helm-managed objects are left out, so the library doesn't fight Helm. `catalog.yaml` records each release by chart, chart version and app version, read from the objects' `helm.sh/chart` and `app.kubernetes.io/version` labels.
- **Read-only is enforced, not assumed.** The wizard and every capture run a SelfSubjectRulesReview and refuse an identity that can write or read Secrets.
- **Lab RBAC scope:** the lab's `scriven-readonly` ClusterRole grants built-in API groups only, so CRDs (Traefik, Gateway API and so on) are unreadable and are listed that way in the catalog. Real clusters will need a decision on which CRD groups to grant.
- **Branches:** `capture/<env>/<id>/<time>`. A new capture closes older open PRs for the same target. If the cluster still matches `main`, nothing is committed.
- **Auto-merge vs repo-wide approvals** (decided 2026-11-17): Git hosts set required approvals per branch, so an auto-merged environment cannot share `main` with one that needs approvals. `merge_policy.<env>.branch` gives it its own base branch, which Scriven creates on first use as an orphan holding only a README, so it never carries stale copies of other environments. The wizard sets `<env>-library` automatically when it is needed. Capture PRs, the watcher's observed branch (`observed-<base>`) and its PR all follow the base branch. The merge check tests protection per branch; a branch only auto-merged environments use may stay unprotected. A bypass for Scriven's account was rejected: it would let Scriven merge into the reviewed branch.
- **Generic `git` provider:** pushes branches only, with no PRs or protection checks. GitHub and GitLab API code is written but has not been run against real repos.

## Online versions

- Blueprint doc: https://claude.ai/artifact/VvWoiwkgH63uiPnBLA7zbY
- Whiteboard notes doc: https://claude.ai/artifact/U359NVpaYNbwaWWCNXx6dx
- Whiteboard: https://claude.ai/artifact/2k5PT6qfjPKLcHDGrdw9AF

The online Blueprint and Whiteboard still describe the tfk8s / manifest route for Rancher. Update them when convenient.

## Phase 3 build: watcher (2026-11-16)

- **Feed:** Kubernetes informers per captured kind, using the same `Filter.Decide` as a full capture. Changes are compared by a hash of what a capture would write, so status, `resourceVersion`, `managedFields`, the revision annotation and HPA-owned `replicas` are ignored.
- **Single-resource re-capture:** only changed objects go through import, generate, clean-up and the zero-diff plan. Their blocks are merged into the library files. Every capture path writes one canonical sorted layout (`joinBlocks`), so watcher commits and full sweeps produce byte-identical files. A sweep straight after the watcher has run reports "up to date".
- **Library flow:** the `observed` branch gets one commit per resource, and one PR `observed` → `main` stays open. Once everything on `observed` is merged, it restarts from `main`. If it can't merge `main` cleanly, it resets to `main` with a force push, and the next sweep re-detects anything still different. Two writers (the in-cluster watcher and its nightly sweep, or two watchers) can push to the same observed branch: a rejected push fetches, rebases its commits onto the new ones and retries up to 3 times. A commit with the same change as the other writer's drops out of the rebase; if the same lines changed on both sides, the push gives up and the next sweep records what still differs. Verified in the lab on 2026-11-18 with the in-cluster watcher and a local one racing on `observed-dev-library`.
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
- **Not done yet:** an egress NetworkPolicy (addresses are site-specific). Image publishing and CI: done 2026-11-17, see below.

## Phase 2 build: AWS capture (2026-11-17)

- **No AWS sign-up:** the user did not want an AWS or LocalStack account. AWS is developed against **Moto** (open source, no token) in the lab. Nothing here has run against a real AWS account, so treat real-AWS behaviour as unproven.
- **Target shape:** one target is one account and region. Settings: `region`, then either `profile`, or `access_key_id` + `secret_access_key` as secret references, or neither (default credential chain). `endpoint` is for emulators only.
- **Same engine as Kubernetes:** AWS SDK lists resources, then import blocks and `terraform plan -generate-config-out`, clean-up, and the zero-diff plan. Version 0.3.0.
- **Covered types (25):** VPC, subnet, internet gateway, route table and association, security group, Elastic IP, NAT gateway, EC2 instance, load balancer, listener, target group, S3 bucket and versioning, IAM role, inline role policy, customer-managed policy, role-policy attachment, DynamoDB table, SNS topic, RDS DB instance and subnet group, Lambda function, KMS key and alias. The 11 added on 2026-11-17 are tested on Moto only. Everything else is uncaptured and `catalog.yaml` says so.
- **Left out on purpose:** the default VPC, each VPC's main route table and default security group, and service-linked roles. AWS creates them and Terraform can only adopt them. Risk: routes someone added to a main route table are not captured.
- **References instead of IDs:** clean-up replaces a captured resource's ID, where another resource uses it, with a reference (`aws_vpc.shop.id`). Opaque IDs and ARNs are replaced anywhere. Names (role name, bucket name) are replaced only in the attribute that means them (`role`, `bucket`). A reference that would close a cycle stays a literal. This is what makes replay into another account work.
- **Read-only check:** first choice is `iam:SimulatePrincipalPolicy` over 26 write and secret-read actions. If that call is not available, the fallback is EC2 dry-run requests, recorded in `catalog.yaml` as the weaker check. The dry-run probes only ever "delete" IDs that do not exist: Moto ignores `DryRun` on some calls, and an earlier probe (`CreateVpc`) really created a VPC in the emulator.
- **Data reads** (decided 2026-11-17): the simulation also covers 7 data reads (`s3:GetObject`, `dynamodb:GetItem`/`Query`/`Scan`, `sqs:ReceiveMessage`, `ssm:GetParameter`, `logs:GetLogEvents`). By default an identity that has them is accepted with a warning, and `catalog.yaml` lists them under `source.data_reads`, because AWS's own `ReadOnlyAccess` grants them. A target with `metadata_only: true` refuses such an identity, and also refuses when only the dry-run fallback could run, since that cannot see data reads.
- **SQS dropped for now:** the provider cannot import a queue whose URL is not an `amazonaws.com` URL, so it cannot be tested on an emulator.
- **Emulator plumbing, kept out of the library:** with `endpoint` set, the scratch directory gets an override file (`s3_use_path_style`), and Terraform runs with `HTTP_PROXY` pointing at the emulator. The provider calls S3 Control at `<account-id>.<host>`, which does not resolve locally; sent through the emulator as a proxy, the request arrives.
- **Lab Moto is patched:** `scriven-lab/moto/Dockerfile` pins 5.2.5 and makes S3 Control use Moto's S3 signer. Without it, with IAM enforcement on, reading bucket tags fails for every identity and no bucket can be imported.
- **IAM is global:** a target with `global_resources: false` skips IAM. Validation refuses two targets that log in the same way (same profile, keys, role and endpoint) and would both capture IAM. Targets that reach the same account through different logins are not detected.
- **Live watching (2026-11-17):** see "AWS live watching" below. Before that, AWS targets were sweep-only.
- **Not done yet:**
  - More resource types: Auto Scaling groups, launch templates, Aurora clusters, listener rules, SQS (see above), ECS/EKS, CloudFront, Route 53.
  - Generated code keeps every `false` default, so it is wordier than a person would write.

## Hardening sprint (2026-11-17)

- **Slow captures were mostly a re-download.** Every capture starts in an empty directory with no lock file, and Terraform then ignores its plugin cache and downloads the provider again (800 MB for AWS). Fix: the first full lock file Terraform writes for a provider (one with `zh:` checksums for every platform) is saved beside the cache in `locks/`, and later captures start from it. `terraform init` went from about 2 minutes to 30 seconds, with the same lock file and `main.tf` as before. `TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE` was tried and rejected: it writes a lock with only this platform's checksum, which would break `terraform init` on any other OS.
- **Provider pinning:** a saved lock pins that provider version on this machine. Delete `<user cache>/scriven/terraform-plugins/locks/<provider>.lock.hcl` to move to a newer provider.
- **What is still slow on the Windows host:** each Terraform command starts the 800 MB AWS provider, and Windows Defender scans it (the first start took 67 seconds, later ones about 10). An AWS capture still takes about 3.5 minutes there. Not changed: a Defender exclusion is a decision for the machine's owner.
- **Image:** pre-fetches `hashicorp/aws` as well as `hashicorp/kubernetes`, both served from the read-only filesystem mirror. `SCRIVEN_PLUGIN_CACHE=off` in the image stops scriven adding a plugin cache, which would copy the providers into `/tmp`.
- **Autoscaled workloads:** a capture leaves out `spec.replicas` on a Deployment or StatefulSet that an HPA scales (the provider marks it optional and computed, so the plan stays at zero diff). A full capture works this out from the HPAs it discovers; the watcher uses its HPA informer. When an HPA is added, edited or removed, the watcher re-checks the workload it scales (before and after), so that workload is re-captured with or without `spec.replicas` straight away.
- **catalog.yaml on watcher commits:** each watcher commit updates `resource_count` and sets `observed_at`. The rest (uncaptured objects, provider versions) stays as the last full capture or sweep wrote it.
- **Metrics:** `scriven watch` serves Prometheus text at `/metrics` on the `watch.audit_listen` address, written by hand (no client library). Counters: changes recorded by label, record errors, sweeps by result, audit events, failed notifications. Gauges: up, targets by mode, per-target watch state, last sweep duration and time, last successful sweep time, resources recorded by the last sweep. The sweep CronJob is a separate process and exports nothing.

## First run against real AWS (2026-11-17)

- **Setup:** a sandbox account, provisioned by the user from the console with the CloudFormation template `scriven-lab/aws-real/shop-sample.yaml` (no AWS CLI). It holds the same sample resources as the Moto lab and a metadata-only `scriven-readonly` user that may also call `iam:SimulatePrincipalPolicy`. The access key goes into the lab Vault at `secret/scriven/aws-real`, typed by the user. Keys are never pasted into chat.
- **Result:** IAM policy simulation proved the identity read-only (the full check, which Moto cannot do), and the capture passed the zero-diff plan. AWS was reachable from this machine with no proxy.
- **Found on real AWS, fixed:**
  - CloudFormation's `cf-templates-<random>-<region>` upload bucket was captured. It is now left out and listed.
  - The account ID survived inside strings: policy resources (`arn:aws:s3:::bucket/*`), the SNS default policy, bucket names. Clean-up now writes a captured resource's ARN at the start of a string as `${aws_x.y.arn}`, the account as `data.aws_caller_identity.current.account_id` (`providers.tf` then declares the data source), and the region inside ARNs as `var.aws_region`. Moto never showed this: its replay account has the same fake ID.
- **CloudFormation-managed resources are left out** (decided 2026-11-17), as Helm-managed objects are: the stack template is their code, and the library would otherwise fight the stack. Discovery lists every live stack's resources (`cloudformation:ListStacks`/`ListStackResources`) and drops items whose ID, alternative ID (an Elastic IP's address) or owner matches a physical resource ID. Owned items (a role's policies, a route table's associations, a bucket's versioning, a key's aliases, a load balancer's listeners) go with their owner. `catalog.yaml` lists the stacks with a count each. If the identity may not read CloudFormation, everything is captured and the catalog says why. The real sandbox is entirely stack-made, so a capture there now finds only what was added by hand.
- **Not yet tested on real AWS:** replay into a second, empty account.

## Decisions and AWS coverage sprint (2026-11-17)

- **Four open questions settled** (details in the sections above): Helm releases recorded by chart and version only; CloudFormation-managed resources left out; data reads warned about by default and refused with `metadata_only`; auto-merged environments get their own library branch.
- **Assume-role:** `role_arn` and `external_id` on an AWS target. Scriven assumes the role for an hour (session name `scriven`) and gives Terraform the temporary credentials in its environment, so the library's provider block stays account-neutral. The read-only check simulates the role's policies.
- **New types, and what their clean-up needed:**
  - Empty nested blocks, and blocks that only say `enabled = false` (an ALB's `access_logs`), are removed: the provider rejects them for missing required arguments.
  - `aws_lb`: `subnet_mapping` blocks that only repeat `subnets` are dropped (the provider accepts one or the other).
  - `aws_instance`: `primary_network_interface` and `private_ip` dropped (AWS-assigned). `aws_nat_gateway`: `private_ip` dropped. `aws_eip`: `network_interface` and `associate_with_private_ip` dropped, because the attachment belongs to the NAT gateway or instance.
  - `aws_db_instance`: `option_group_name` and `parameter_group_name` dropped when they name an AWS default group (the name is version-specific and does not exist in a fresh account). The master password is never captured; replay must supply one.
  - `aws_lambda_function`: code is never read. Clean-up adds `filename = "${path.module}/lambda/<name>.zip"` with `ignore_changes = [filename]`.
  - `aws_lb_target_group`: `lambda_multi_value_headers_enabled` and `proxy_protocol_v2` go from unset to `false` on the first plan after an import. They are accepted as notes, like Terraform-only settings. `cookie_duration = 0` is kept.
  - Auto Scaling-launched instances, Aurora cluster members, AWS-managed KMS keys and the default DB subnet group are left out and listed.
- **Lab Moto now carries five patches** (`scriven-lab/moto/Dockerfile`): S3 Control signing (earlier); Lambda GET requests failing signature checks under IAM enforcement (Moto signed the headers as if they were the body); NAT gateways missing `availabilityMode`, which provider 6.x reads as a regional NAT gateway with no subnet; instances launched with a network-interface spec reporting no security groups; and `ModifyInstanceAttribute` for the shutdown behaviour falling through to the security-group handler and wiping the instance's groups (found because the replay, unlike the seed, sets that attribute).
- **Real sandbox:** its `scriven-readonly` user predates the new types, so ELB, RDS, Lambda, KMS and CloudFormation show as unreadable there until the stack is updated with the new template. The new types have not been created in real AWS, because NAT gateways, load balancers and RDS cost money by the hour.

## CI (2026-11-17)

- **GitHub Actions** (`.github/workflows/ci.yml`): on every push to `dev` or `main` and every PR, `gofmt -l`, `go vet`, `go test -race` and a build. On a push to `main` or a `v*` tag, the image is built and pushed to `ghcr.io/<owner>/<repo>` (tags: short SHA, `latest` on main, the version on tags), using `GITHUB_TOKEN` and the Actions build cache. `dev` pushes skip the image because it downloads about 1 GB of providers.
- **Unit tests need no Terraform, cluster or emulator**, so CI runs them on a bare runner. The lab acceptance checks stay manual.
- Verified before the first run: the same steps pass in `golang:1.27` on Linux, including the race detector (this Windows host has no C compiler for `-race`).

## AWS live watching (2026-11-17)

- **Feed:** CloudTrail `LookupEvents` (event history), polled every `watch.aws_poll` (default 60s), filtered to `ReadOnly=false` and to the services scriven captures. Failed calls (`errorCode` set) are dropped. Calls whose user agent is CloudFormation are skipped: a stack doing its own work changes only resources the capture leaves out, and on the real sandbox one such call cost a two-minute re-capture that found nothing. IAM events live in `us-east-1`, so a target that captures global resources reads that region too, for IAM only. No trail, EventBridge rule or queue is needed, so nothing is created in the account and it costs nothing. EventBridge → SQS would cut delivery to seconds; it was left as a later option because it needs resources created in the account.
- **Delivery delay:** CloudTrail shows calls after a few minutes, and not always in order. Each poll looks back 20 minutes and skips event IDs already seen.
- **Recording:** a burst of calls (quiet for `watch.debounce`) triggers one full capture of the target, rather than per-resource re-capture: AWS code is full of cross-references, which only a full capture links correctly. The result is compared with the observed branch, and each differing resource gets its own commit and notification.
- **Attribution:** each event's identifiers (every string in its request and response, plus the resources CloudTrail lists) are matched against the import ID of each changed resource. They match exactly, or as its owner (role → its policies, route table → its associations), or as an ARN ending in the ID. The latest matching call names the caller: `user/alice`, `assumed-role/admin/bob`, `root`, or the AWS service acting. Its user agent names the tool (console, Terraform, aws-cli...). A change no call names is recorded as "unknown".
- **Fallback:** if `LookupEvents` fails at start-up (no permission, or Moto, which does not implement it), the target is sweep-only, as before, and the log says why.
- **Read-only check:** `cloudtrail:StopLogging` and `cloudtrail:DeleteTrail` added to the refuse list.
- **Lab:** cannot be tested on Moto. The real sandbox needs the updated template (adds `cloudtrail:LookupEvents`).

## Watcher fixes after the real-AWS test (2026-11-18)

- **Real sandbox, after the template update:** the read-only check ran the full IAM policy simulation (44 actions) and passed; the capture left out the stack's 15 resources and captured the one bucket outside it at zero diff; the watcher started following CloudTrail.
- **CloudFormation's own calls are skipped** by the AWS watcher (see AWS live watching).
- **HPA changes re-check the workload they scale,** both the old and new `scaleTargetRef`, so the Deployment is re-captured with or without `spec.replicas` at once. Tested in the lab: adding an HPA to `shop/web` recorded the HPA and the Deployment without `replicas`. The in-cluster image (older) recorded only the HPA, which showed the gap.
- **Observed-branch pushes retry after a rejection** (see Phase 3, library flow).
