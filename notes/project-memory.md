# Scriven: project memory

What has been decided about this project so far. Started on 2026-11-16 and last updated the same day.

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
