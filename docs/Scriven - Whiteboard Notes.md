# Scriven — Whiteboard Notes

Nov 16, 2026 · @Kay

Capture deployed infra once, replay anywhere with Terraform. Scriven reads four platforms, runs every capture through one six-step pipeline into a Git-backed IaC library, and keeps an always-on, read-only watcher that flags drift.

## Sources and native exporters

Each platform is read with its own exporter or inventory, and all of them feed the capture pipeline.

| Platform | Exporter |
| --- | --- |
| Rancher on-prem | `rancher2` provider + cleaned K8s manifests (tfk8s) |
| Azure | aztfexport (Microsoft) |
| GCP | `gcloud resource-config bulk-export` |
| AWS | AWS Config / Resource Explorer inventory |

## Capture pipeline

Six steps in a straight line; step 5 is the gate.

1. **Discover**: list resources and their IDs per platform.
2. **Import blocks**: one `import {}` block per resource.
3. **Generate HCL**: `terraform plan -generate-config-out`.
4. **Clean up**: drop computed fields, add variables and modules.
5. **Verify**: `terraform plan` must show zero changes.
6. **Commit**: versioned into the IaC library.

## Watcher mode (always on, read-only)

Two inputs feed one watcher, which re-captures only what changed and commits the result to the library.

**Inputs**

- **Change feeds**: EventBridge, Event Grid, Asset feeds, K8s watch.
- **Nightly sweeper**: a rate-limited full scan that catches missed events.

**Flow**

1. **Watcher**: stateless Go service with read-only identities.
2. **Re-capture**: only the changed resource.
3. **Diff vs library**: drift or expected change?
4. **Notify + PR**: Slack, Teams or email with the diff and who changed it, then commit to the library.

## IaC library

Both the capture pipeline and the watcher commit here.

- **Git repo**: laid out as `/envs/<env>/<platform>`.
- **`catalog.yaml`** per capture: owner, date, source account, provider versions.
- **Scheduled re-capture**: diff against the library = drift alert.

## Gotchas

- Secrets aren't exported: use vault references.
- Generated code is flat: refactor into modules.
- Skip K8s objects owned by operators or controllers.
- Pin provider versions for faithful replay.
