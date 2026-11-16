# Scriven

Captures deployed infrastructure (Rancher on-prem, AWS, Azure, GCP) as Terraform, keeps it in a Git-backed IaC library, and watches read-only for drift.

**Status:** Phase 1 is done for Rancher/Kubernetes. The setup wizard works and the one-shot capture passes the zero-diff and replay checks in the lab. AWS, Azure and GCP (Phase 2) and the watcher (Phase 3) are not built yet.

## What's here

| Path | What it is |
| --- | --- |
| `cmd/scriven/` | The CLI entry point |
| `internal/config/` | `scriven.yaml`: types, validation, `config set` |
| `internal/wizard/` | `scriven init` / `config edit`: the four-step setup wizard |
| `internal/checks/` | The live test behind each wizard step (reused by `config validate`) |
| `internal/k8s/` | Read-only Kubernetes access, including the "is this identity really read-only?" check |
| `internal/capture/` | The six-step pipeline: discover, import blocks, generate HCL, clean up, verify, commit |
| `internal/library/` | Git working copy, PRs and branch protection (Gitea tested; GitHub and GitLab written but untested) |
| `internal/notify/` | Slack, Teams (Adaptive Card) and email |
| `internal/secrets/` | Resolves `vault://`, `env://` and `file://` references |
| `docs/` | Blueprint and whiteboard notes |
| `notes/project-memory.md` | Decisions made so far |
| `scriven-lab/` | Docker test lab and the scripts that seed it and run the replay check |

## Build

Needs Go 1.25+, Terraform 1.5+ and git on `PATH`.

```bash
go build -o bin/scriven ./cmd/scriven     # bin/scriven.exe on Windows
go test ./...
```

## Commands

| Command | Does |
| --- | --- |
| `scriven init` | Runs the wizard and writes `scriven.yaml`. Every answer is tested live before it is kept |
| `scriven config edit` | Re-runs the wizard with the current answers filled in |
| `scriven config set <key> <value>` | Changes one setting, e.g. `notify.channels "[teams, email]"`. The setting is tested first, and the old value stays if the test fails |
| `scriven config validate` | Re-tests every saved setting |
| `scriven capture [target]` | Captures the pilot (or the named target), checks for zero diff, opens a library PR and notifies |
| `scriven capture --out DIR` | Same, but writes the files to `DIR` instead of committing |

`vault://` references need `VAULT_TOKEN` in the environment. The address comes from `vault.addr` in `scriven.yaml` or from `VAULT_ADDR`.

## How a Rancher capture works

1. **Discover:** connects with the read-only identity and **refuses to go on** if that identity can write anything or read Secrets. It then lists every resource kind and skips system namespaces, objects owned by controllers, built-in k3s/RKE2/Rancher objects and Helm-managed objects.
2. **Import blocks:** writes one `import {}` per object, into the provider's typed resources (`kubernetes_deployment_v1` and so on). Kinds with no typed resource are recorded as not captured.
3. **Generate HCL:** runs `terraform plan -generate-config-out`.
4. **Clean up:**
   - Sorts the blocks so output is stable.
   - Removes empty and zero values (some of these fail the provider's own validation) and values the cluster assigns, such as Service cluster IPs.
   - Corrects a provider misread: see the [notes](notes/project-memory.md).
5. **Verify:** `terraform plan` against the live cluster must show no changes. The only differences accepted are Terraform-only settings such as `wait_for_rollout`, and the known provider misread once it has been checked against the live object. Both are listed in `catalog.yaml`.
6. **Commit:** writes `envs/<env>/<platform>/<id>/` to a branch `capture/<env>/<id>/<time>`, opens a PR, closes older open PRs for the same target, applies the merge policy and sends notifications. If nothing changed, nothing is committed.

## Lab quick start

```bash
cd scriven-lab
cp .env.example .env        # set the Rancher bootstrap password
./lab-up.sh                 # add "aws" and/or "gcp" for the optional emulators
./lab-seed.sh               # Gitea user/repo/branch protection, Vault secrets, read-only kubeconfigs
export VAULT_TOKEN=root
../bin/scriven init         # lab-seed prints the answers to give
../bin/scriven capture
./lab-replay.sh <dir>       # apply a capture (from `capture --out <dir>`) to a fresh cluster and re-capture it
./lab-drift.sh              # simulate manual changes (for the Phase 3 watcher)
./lab-down.sh               # --wipe also removes data volumes
```

Needs Docker with Compose, k3d, kubectl and helm. Lab credentials go in `scriven-lab/.lab/`, which git ignores.

## Lab acceptance checks (Blueprint)

- [x] Wizard completes and every step's test passes against lab services
- [x] Full capture of `prod-sim` passes the zero-diff check
- [x] Replay of that capture onto a fresh k3d cluster re-captures with no differences (`lab-replay.sh`)
- [ ] `lab-drift.sh` produces 3 notifications and an `observed` PR in Gitea (needs the Phase 3 watcher)
- [x] The tool's service account is refused when it tries to read a Secret or write anything
