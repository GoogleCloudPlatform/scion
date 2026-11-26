---
title: Move-Bundle Cross-Provider E2E Checklist
description: Manual end-to-end verification checklist for cross-broker bundle transfer (hub.agent_move_bundle) across Docker, Kubernetes NFS, and Cloud Run NFS, including refused cases, failure injection, and staging cleanup.
---

Use this checklist to verify cross-broker bundle transfer (`scion reincarnate <agent> --broker <target>`, `transferMode: "bundle"`) end-to-end across compute providers before graduating [`hub.agent_move_bundle`](/scion/reference/experiments/#server-experiments-with-notes) or cutting a release that touches move-bundle code.

For user and operator documentation of the transfer-mode ladder, limits, exclusions, and refusal reasons, see [Moving an Agent to Another Runtime Broker](/scion/hosted/ha/multi-broker/#moving-an-agent-to-another-runtime-broker).

## Prerequisites

- A Hub configured with GCS object storage (`server.storage.provider: gcs`, bucket `BUCKET_NAME`, Hub ID `HUB_ID`) and the recommended 7-day lifecycle rule on `move-bundles/` (see [Deploy on GCP — 1c. GCS Bucket](/scion/hosted/ha/setup-gcp/#1c-gcs-bucket)).
- The `hub.agent_move_bundle` experiment enabled on the Hub (and `hub.k8s_nfs_home` enabled whenever testing Kubernetes NFS in pairs 2 and 3, with each Kubernetes agent started after enabling it):
  ```bash
  scion hub experiments enable hub.agent_move_bundle
  # Required for Kubernetes NFS pairs (2 and 3):
  scion hub experiments enable hub.k8s_nfs_home
  ```
- Online Runtime Brokers for the provider pairs under test, each linked to the test project and advertising `agentMove` and `agentMoveBundle`:
  ```bash
  scion hub brokers info <broker-name> --json | jq '{name, status, capabilities: {agentMove: .capabilities.agentMove, agentMoveBundle: .capabilities.agentMoveBundle}}'
  ```
- A Hub-managed git project using `clone-per-agent` (`--workspace-mode per-agent`) and a Hub-managed non-git project using `empty-per-agent` (`--workspace-mode per-agent` without a git remote).
- A file-type Hub secret scoped to the test project (for example targeting `~/.config/scion-e2e/secret.txt`) so you can verify that injected secrets are excluded from the bundle and re-injected on the target.
- A handoff file that instructs the new generation to stay idle so the agent does not modify `/workspace` or `$HOME` before verification runs:
  ```bash
  printf 'Do not modify any files in /workspace or $HOME. Run `sciontool status idle "awaiting E2E verification"` and wait.\n' > /tmp/move-handoff.md
  ```

## Standard Source Seed & Target Verification

Run these scripts inside the source agent before each happy-path move, and run the verification script inside the target agent after the move completes (before sending any work that modifies `/workspace` or `$HOME`).

**How to run scripts in an agent container:**
- Where Conduit SSH is enabled (`hub.conduit`): pipe the heredoc through `scion ssh <agent> -- bash -euo pipefail -s` (shown below).
- **Docker / Podman fallback:** `docker exec -i <container> bash -euo pipefail -s` (or `scion attach <agent>` and paste the body).
- **Kubernetes fallback:** `kubectl exec -i <pod> -n <namespace> -- bash -euo pipefail -s` (or `scion attach <agent>`).
- **Cloud Run fallback:** when SSH and exec are unavailable, use `scion attach <agent>` to run the checks interactively, or inspect the agent's `workspace/` and `home-<agentID>/` directories directly on the Runtime Broker's NFS mount.

### Seed state on source agent

```bash
scion ssh <agent> -- bash -euo pipefail -s <<'EOF'
cd /workspace
# 1. Unpushed git commit (skip for empty-per-agent projects)
if [ -d .git ]; then
  echo "unpushed-commit-data" > unpushed.txt
  git add unpushed.txt
  git -c user.name="E2E" -c user.email="e2e@example.com" commit -m "e2e unpushed commit"
fi

# 2. Uncommitted change, untracked file, modes, empty dir, symlinks
echo "uncommitted-edit" >> README.md
echo "untracked-content" > untracked.txt
printf '#!/bin/sh\necho ok\n' > run.sh && chmod 0755 run.sh
printf 'private-key-material\n' > private.key && chmod 0600 private.key
mkdir -p empty-dir
ln -s untracked.txt internal-link.txt
ln -s /etc/hosts escaping-link.txt

# 3. $HOME files: preserved vs excluded (plant unique source markers so targets
#    that legitimately inject their own ADC/config still pass verification)
printf 'echo move-bundle-history-marker\n' >> "$HOME/.bash_history"
printf 'keep-me-in-home\n' > "$HOME/notes.txt"
mkdir -p "$HOME/.config/gcloud" "$HOME/.docker" "$HOME/.kube" "$HOME/.cache"
printf 'source-adc-do-not-copy\n' > "$HOME/.config/gcloud/application_default_credentials.json"
printf '{"auths":{"source-docker-marker":{}}}\n' > "$HOME/.docker/config.json"
printf 'apiVersion: v1\n# source-kubeconfig-marker\n' > "$HOME/.kube/config"
printf 'cache-bytes\n' > "$HOME/.cache/e2e-cache.bin"

# Record source token digest so we can verify it does not travel
sha256sum "$HOME/.scion/scion-token" | awk '{print $1}'
EOF
```

Save the printed source token SHA-256 on your workstation:

```bash
export SRC_TOKEN_SHA="<paste-sha256-here>"
```

### Verify state on target agent

Pass `$SRC_TOKEN_SHA` as `$1` so no local shell expansion happens inside the heredoc:

```bash
scion ssh <agent> -- bash -euo pipefail -s -- "$SRC_TOKEN_SHA" <<'EOF'
SRC_TOKEN_SHA="$1"
cd /workspace

# 1. Workspace content, modes, empty dir, symlinks
if [ -d .git ]; then
  git log -1 --oneline | grep -q "e2e unpushed commit"
  test -f unpushed.txt
fi
grep -q "uncommitted-edit" README.md
grep -q "untracked-content" untracked.txt
test "$(stat -c %a run.sh)" = "755"
test "$(stat -c %a private.key)" = "600"
test -d empty-dir
test "$(readlink internal-link.txt)" = "untracked.txt"
test ! -L escaping-link.txt

# 2. Preserved $HOME files
grep -q "move-bundle-history-marker" "$HOME/.bash_history"
grep -q "keep-me-in-home" "$HOME/notes.txt"

# 3. Source-only credentials and caches did NOT travel from source
#    (uses marker checks so targets that inject their own ADC/config still pass)
! grep -qs "source-adc-do-not-copy" "$HOME/.config/gcloud/application_default_credentials.json"
! grep -qs "source-docker-marker" "$HOME/.docker/config.json"
! grep -qs "source-kubeconfig-marker" "$HOME/.kube/config"
test ! -e "$HOME/.cache/e2e-cache.bin"

# 4. Target issued its own Hub token and re-injected secrets
test -f "$HOME/.scion/scion-token"
test "$(sha256sum "$HOME/.scion/scion-token" | awk '{print $1}')" != "$SRC_TOKEN_SHA"
test -f "$HOME/.config/scion-e2e/secret.txt"
echo "TARGET VERIFICATION PASSED"
EOF
```

## Provider-Pair Checklists

### 1. Docker ↔ Docker

Covers local-disk Runtime Brokers (`docker`, `podman`, or Apple `container`) that do not share an NFS export.

- [ ] **Dry run selects `bundle`:**
  ```bash
  scion reincarnate <agent> --broker <docker-b> --dry-run
  ```
  Expected output includes:
  - `Transfer mode:` `bundle (workspace and home are copied through hub storage (~...))`
  - `workspace_storage_reported`, `same_export`, `workspace_on_export` marked `waived` (without `(--force)`).
  - `bundle_experiment`, `workspace_bundleable`, `bundle_storage`, `capability`, `bundle_source` marked `passed`.
  - Consequences show `workspace` and `home` carried, `harness-session` and `credentials` recreated, and no `local-work` loss.
- [ ] **`--force` still selects `bundle` (never downgrades to `recreate`):**
  ```bash
  scion reincarnate <agent> --broker <docker-b> --dry-run --force
  ```
  Expected output includes `Transfer mode: bundle (...)` and:
  `--force was not needed: the workspace and home are copied through hub storage.`
- [ ] **Execute Docker A → Docker B (`clone-per-agent`):**
  ```bash
  scion reincarnate <agent> --broker <docker-b> --handoff-file /tmp/move-handoff.md
  ```
  Wait for the agent to reach `running` on `<docker-b>` (`scion list`), then run the **Verify state on target agent** block.
- [ ] **Execute Docker B → Docker A (return leg):** repeat `--dry-run`, real move, and target verification back to `<docker-a>`.
- [ ] **`empty-per-agent` project:** repeat `--dry-run`, real move, and target verification for an agent in a non-git `empty-per-agent` project.
- [ ] **Staging cleanup:** verify the GCS staging prefix is empty (see [Staging Cleanup & Orphan Sweep](#staging-cleanup--orphan-sweep)).

### 2. Docker ↔ Kubernetes (NFS)

Covers moving between a local-disk Runtime Broker (`docker`) and a Kubernetes Runtime Broker configured with `hub.k8s_nfs_home` enabled (with the agent started after enabling it) and `server.project_storage` with `backend: nfs`, `homes: nfs`, `homes_options.allow_incomplete_phases: true` (see [Agent home storage](/scion/reference/server-config/#agent-home-storage-serverhome_storage)), and a share with `pv_name`.

- [ ] **Docker → Kubernetes (NFS):**
  ```bash
  scion reincarnate <agent> --broker <k8s-nfs-broker> --dry-run
  scion reincarnate <agent> --broker <k8s-nfs-broker> --handoff-file /tmp/move-handoff.md
  ```
  - Dry run reports `Transfer mode: bundle (...)`.
  - After start on `<k8s-nfs-broker>`, run the **Verify state on target agent** block.
  - Confirm `home-prepare` ran in seed-over mode (target-rendered harness config and `agent-info.json` are present alongside preserved `~/notes.txt` and `~/.bash_history`) and file ownership inside the pod allows the container user (`uid 1000`) to read and write both `/workspace` and `$HOME`.
- [ ] **Kubernetes (NFS) → Docker:**
  - Seed new markers on the Kubernetes NFS agent (including verifying an existing `/scion-staging/...` secret symlink in NFS home does not fail precheck or export).
  ```bash
  scion reincarnate <agent> --broker <docker-a> --dry-run
  scion reincarnate <agent> --broker <docker-a> --handoff-file /tmp/move-handoff.md
  ```
  - Dry run reports `Transfer mode: bundle (...)`.
  - After start on `<docker-a>`, run the **Verify state on target agent** block.
- [ ] **Staging cleanup:** verify the GCS staging prefix is empty.

### 3. Kubernetes (NFS) ↔ Cloud Run (NFS)

Covers moving between a Kubernetes NFS Runtime Broker (`hub.k8s_nfs_home` enabled, `homes: nfs`, `homes_options.allow_incomplete_phases: true`, `pv_name`) and a Cloud Run Instances Runtime Broker configured with NFS `server.project_storage` (`backend: nfs`, `nfs.mount_root`, `shares[0]`).

- [ ] **Kubernetes (NFS) → Cloud Run (NFS):**
  ```bash
  scion reincarnate <agent> --broker <cloudrun-nfs-broker> --dry-run
  scion reincarnate <agent> --broker <cloudrun-nfs-broker> --handoff-file /tmp/move-handoff.md
  ```
  - Dry run reports `Transfer mode: bundle (...)`.
  - After start on `<cloudrun-nfs-broker>`, run the **Verify state on target agent** block.
  - Confirm `provisionCloudRunHome` seeded target-rendered home files over the imported leaf without overwriting `~/notes.txt` or `~/.bash_history`, and workspace/home ownership matches `nfs.uid:nfs.gid`.
- [ ] **Cloud Run (NFS) → Kubernetes (NFS):**
  - Seed new markers on the Cloud Run NFS agent.
  ```bash
  scion reincarnate <agent> --broker <k8s-nfs-broker> --dry-run
  scion reincarnate <agent> --broker <k8s-nfs-broker> --handoff-file /tmp/move-handoff.md
  ```
  - Dry run reports `Transfer mode: bundle (...)`.
  - After start on `<k8s-nfs-broker>`, run the **Verify state on target agent** block.
- [ ] **Staging cleanup:** verify the GCS staging prefix is empty.

## Refused Cases & Ladder Precedence

Run each case with `--dry-run` and confirm the move is refused (or selects `export`) with no side effects on the running agent:

- [ ] **Same-export eligible move still selects `export` (even with `--force`):**
  ```bash
  scion reincarnate <k8s-agent> --broker <same-export-k8s-broker> --dry-run
  scion reincarnate <k8s-agent> --broker <same-export-k8s-broker> --dry-run --force
  ```
  Expected: `Transfer mode: export (the workspace is carried through the shared NFS export)` and `--force was not needed: the workspace is carried through the shared NFS export.`
- [ ] **Plain Kubernetes pod (non-NFS workspace, pod-local home, or `hub.k8s_nfs_home` off when agent started):**
  ```bash
  scion reincarnate <plain-k8s-agent> --broker <docker-b> --dry-run
  ```
  Expected: refused with `bundle transfer unavailable: workspace and home live only in the pod and are deleted on stop` (or `agent home lives only in the pod and is deleted on stop`, or `broker <name> does not support bundle move; upgrade it` when the Runtime Broker has no NFS home configured), plus the `--force` hint.
- [ ] **Cloud Run Sandbox:**
  ```bash
  scion reincarnate <sandbox-agent> --broker <docker-b> --dry-run
  ```
  Expected: refused with `bundle transfer unavailable: Cloud Run Sandbox workspace and home are not durable for bundle moves` (or `broker <name> does not support bundle move; upgrade it` on a sandbox-only Runtime Broker).
- [ ] **Experiment disabled:**
  ```bash
  scion hub experiments disable hub.agent_move_bundle
  scion reincarnate <agent> --broker <docker-b> --dry-run
  scion hub experiments enable hub.agent_move_bundle
  ```
  Expected while off: refused with `bundle transfer unavailable: bundle transfer requires the hub.agent_move_bundle experiment` and the `--force` hint.
- [ ] **Hub object storage unconfigured:**
  On a Hub with no `server.storage` configured:
  ```bash
  scion reincarnate <agent> --broker <docker-b> --dry-run
  ```
  Expected: refused with `bundle transfer unavailable: hub object storage is not configured`.
- [ ] **Non-GCS Hub storage (for example `local`):**
  On a Hub configured with `server.storage.provider: local`:
  ```bash
  scion reincarnate <agent> --broker <docker-b> --dry-run
  ```
  Expected: refused with `bundle transfer unavailable: hub object storage provider "local" cannot issue signed URLs a remote broker can use`.
- [ ] **Shared-plain or worktree-per-agent workspace:**
  ```bash
  scion reincarnate <shared-or-worktree-agent> --broker <docker-b> --dry-run
  ```
  Expected: refused (`workspace_mode` for worktree-per-agent; `bundle transfer supports clone-per-agent and empty-per-agent workspaces; this agent's workspace is <mode>` for shared-plain).
- [ ] **Missing start-time home-exclusion record:**
  For an agent that has not started since `.scion-move-bundle-home.json` recording shipped (or after removing that file on a test Runtime Broker):
  ```bash
  scion reincarnate <agent> --broker <docker-b> --dry-run
  ```
  Expected: refused with `bundle transfer unavailable: restart the agent once to enable bundle moves`.

## Failure Injection & Rollback

### 1. Limit exceeded (precheck before stop, and during export or import)

- [ ] **Caught at precheck / recheck (before stop):** On a test Hub with a small `server.reincarnate.move_bundle.workspace_max_bytes` (or by writing a file larger than the configured limit inside the source agent's `/workspace`):
  ```bash
  scion reincarnate <agent> --broker <target> --dry-run
  ```
  Expected:
  - Refused with `bundle transfer unavailable: bundle exceeds limit: workspace bytes (...) exceed configured limit`.
  - If triggered between dry run and worker start, the worker fails from `pending` before stopping the agent (`TestMoveBundleWorker_PrecheckOverLimitFailsBeforeStop`).
  - The source agent remains **running** on the source Runtime Broker with its workspace intact, and no objects exist under `move-bundles/` in GCS.
- [ ] **Caught during export or import (post-stop):** If a file crosses a limit after precheck (for example written just before stop, or enforced against a forged/oversized manifest during import), export fails from `stopping` with `bundle exceeds limit:` (source intact, staging deleted) and import rolls back to source (target deleted, staging deleted). Because racing precheck vs. stop is timing-sensitive to inject manually, this path is also covered automatically by `pkg/transfer/bundle` T4 (`TestT4_Limits_WriterAndReader`) and Hub T11 (`TestMoveBundleWorker_ManifestVerifySizeMismatch`).

### 2. Export failure (source intact, no target touched)

- [ ] Start a bundle move (`scion reincarnate <agent> --broker <target> --handoff-file /tmp/move-handoff.md`) and restart the **source** Runtime Broker while `scion list` shows `migrating: uploading snapshot` (so the Hub status poll gets `404`).
- [ ] Verify the outcome (also covered by T11 `TestMoveBundleWorker_ExportFailures`):
  - The reincarnation record fails from `stopping` with an error starting with `bundle export failed:`.
  - The agent remains assigned to the **source** Runtime Broker, stopped, in `error` phase, with status message:
    `move failed: <err>; agent state is intact on <src>; \`scion start\` to resume`
  - No target agent directory was created on `<target>`.
  - No staging objects remain in GCS for that reincarnation ID.
  - Running `scion start <agent>` resumes cleanly on the source Runtime Broker with all seeded files intact.

### 3. Import failure → marker-guarded rollback

- [ ] Start a bundle move (`scion reincarnate <agent> --broker <target> --handoff-file /tmp/move-handoff.md`) and inject an import failure on the **target** Runtime Broker while `scion list` shows `migrating: restoring snapshot` (for example, restart the target Runtime Broker quickly so the Hub poll gets `404`, or block target egress to GCS).
- [ ] **Timing caveat:** the Hub polls every 2 seconds and tolerates only 3 consecutive transient transport/5xx errors (~6 seconds total). If the target Runtime Broker stays down longer than ~6 seconds, the move fails on transport errors and rolls back while the target is still unreachable; the Hub then logs `move rollback: could not remove the agent's state on the target broker; left in place`, and target cleanup must be verified (or cleaned up manually) after the target Runtime Broker comes back online.
- [ ] Verify the rollback outcome (also covered by T11 `TestMoveBundleWorker_ImportFailuresRollBack`):
  - The agent is restored to the **source** Runtime Broker in `error` phase, stopped, and its reincarnation record error starts with `bundle import failed:`.
  - When the target Runtime Broker was reachable during rollback (fast restart returning `404`, or GCS egress block), the target has no leftover agent directory and no `.bundle-*` staging siblings (marker-guarded rollback delete cleaned them up). If the target was unreachable during rollback, check the Hub log for `move rollback: could not remove the agent's state on the target broker; left in place` and clean up stale target state after the target returns.
  - Running `scion start <agent>` resumes cleanly on the source Runtime Broker with all seeded workspace and `$HOME` files intact.
  - No staging objects remain in GCS for that reincarnation ID.

### 4. Definitive start failure → marker-guarded rollback

- [ ] Trigger a definitive start failure on the target after import succeeds (for example, pass `--image <nonexistent-image>` with `--broker <target>` so container start fails definitively without leaving a container).
- [ ] Verify the rollback outcome (also covered by T11 `TestMoveBundleWorker_DefinitiveStartFailureRollsBack`):
  - The target agent directory and `.bundle-*` staging siblings are removed by marker-guarded rollback delete.
  - The agent is restored to the **source** Runtime Broker with its previous image/config and source files intact.
  - No staging objects remain in GCS for that reincarnation ID.

### 5. Signed-URL redaction

- [ ] Inspect the failed reincarnation records, Hub logs, and both Runtime Brokers' logs from the failure-injection runs above. Confirm no GCS signed URL query parameters (`X-Goog-Signature`, `X-Goog-Credential`) appear in any log line or `error` field.

## Staging Cleanup & Orphan Sweep

- [ ] **List staging prefix after moves:**
  ```bash
  # When server.hub.hub_id is set:
  gcloud storage ls "gs://${BUCKET_NAME}/hubs/${HUB_ID}/move-bundles/"
  # When server.hub.hub_id is unset:
  gcloud storage ls "gs://${BUCKET_NAME}/move-bundles/"
  ```
  Expected: no objects remain after completed or failed moves.
- [ ] **Orphan sweep (including while experiment is off):**
  1. Upload a dummy object to a synthetic missing-record prefix:
     ```bash
     printf 'orphan-test\n' | gcloud storage cp - "gs://${BUCKET_NAME}/hubs/${HUB_ID}/move-bundles/00000000-0000-0000-0000-000000000000/manifest.json"
     ```
  2. Optionally disable `hub.agent_move_bundle` to confirm cleanup is ungated.
  3. Wait for the next 5-minute `reincarnation-sweep` tick and re-list the prefix.
  4. Expected: the synthetic orphan prefix is deleted (and any concurrently running non-terminal move's prefix is untouched). Re-enable `hub.agent_move_bundle` if disabled.
- [ ] **GCS bucket lifecycle backstop:**
  ```bash
  gcloud storage buckets describe "gs://${BUCKET_NAME}" --format="json(lifecycle)"
  ```
  Expected: a `Delete` rule with `age: 7` matching `hubs/${HUB_ID}/move-bundles/` (and/or `move-bundles/`).
