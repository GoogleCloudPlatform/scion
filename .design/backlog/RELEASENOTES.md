# Release Notes — Scion

## Unreleased

### Fixed
- **K8s attach pod name resolution** — `scion attach` now uses the actual grove-prefixed pod name (e.g., `sciontest--hello`) instead of the bare agent name, fixing GKE Warden `autogke-no-pod-connect-limitation` errors
- **K8s attach su password prompt** — `scion attach` no longer prompts for a password on GKE Autopilot pods that run as non-root with `allowPrivilegeEscalation: false`

### Added
- **Resolved project settings endpoint** — `GET /api/v1/projects/{projectId}/settings/resolved` reports every project setting alongside whether a hub-level default exists. Requires `ActionRead` on the project (not admin-gated), so project owners can see that hub defaults exist. Despite the name it does **not** return an effective value: precedence is owned by the code that applies it, and a second implementation here would drift silently. `hubDefault` is tri-state (`present`/`absent`/`unknown`) — `unknown` means the hub could not determine it, and must not be rendered as "no hub default". See the [reference page](https://googlecloudplatform.github.io/scion/reference/project-settings-resolved/).
- All container images built and published to Artifact Registry (core-base, scion-base, scion-claude, scion-gemini, scion-opencode, scion-codex)

### Changed
- **A project's default count limits now have an explicit Unlimited.** On the project settings
  `PUT`, `defaultMaxTurns` and `defaultMaxModelCalls` now take `0` as Unlimited (an explicit "no
  limit" that new agents get above the template, hub and Runtime Broker defaults) and `null` to
  clear; `0` used to clear. A negative value is refused with `422`. **Reload any open hub web tabs
  after upgrading:** a project settings page loaded before the upgrade sends `0` for an empty count
  limit on every save, which now sets Unlimited for every new agent of the project. The Hub records
  the audit event `project_settings_limit_unlimited` (and logs a warning) for such a change, so it
  can be found and reverted. (ptone/scion#3925)
- **Clearing one resources sub-field now applies at the next start.** A `config` PATCH whose
  `resources` leaves out a sub-field the agent stores (requests or limits `cpu`/`memory`, or `disk`)
  now clears just that sub-field, and the resource defaults fill it again at the next start, on a
  Runtime Broker that advertises `clearedResourceSubfields`. Upgrade the Hub first, then the
  brokers; with an older broker the clear still waits for the next reincarnation. A sub-field drop
  that an older broker held before this change keeps its stale value until the agent is
  reincarnated, because the Hub's inline config no longer holds that value and a later PATCH
  cannot detect the drop. The Hub records such a clear on every broker, so an agent whose broker is
  upgraded after the edit applies it at its next start, which can be earlier than the reincarnation
  the edit was reported as waiting for. (ptone/scion#5676)
- **`SCION_SHARED_WORKSPACE` removed.** The broker no longer sets `SCION_SHARED_WORKSPACE` in agent
  containers, and `sciontool init` no longer reads it. Use `SCION_WORKSPACE_MODE=shared-plain` with
  `SCION_WORKSPACE_GIT=true` to detect a shared git workspace. An agent image with this change,
  started by an older broker that sets neither new variable, is not treated as a shared git workspace,
  so its shared-workspace git setup is skipped. Likewise, an agent image whose `sciontool` predates
  `SCION_WORKSPACE_MODE` (ptone/scion#572) reads only the legacy variable, so on this broker it also
  skips the shared-workspace git setup. Run matching broker and agent image versions.
  (ptone/scion#575)
- **Session-end metrics are kept until sent; mixed agent images can drop unsent ones.** A finished
  session's summary now stays in the agent's session state file (`~/.scion/session-metrics.json`)
  until a send has been attempted, so a report survives the hook or init process being killed and
  is sent later by another hook or by the init daemon. Agent images built before this change do not
  know about these unsent reports: if an agent home last used by a newer image is started on an
  older one (an image rollback, or a staged rollout where some templates or brokers still use older
  images), the older `sciontool` rewrites or deletes the state file without them, and those reports
  are lost. They are never sent twice, because older images never send them. Upgrading from an
  older image to a newer one is safe. To avoid the loss, update all agent images together and avoid
  rolling an agent back across this change; otherwise expect a small loss of session metrics for
  agents that move between image versions. Nothing else in the state file, and no other metrics or
  agent behavior, is affected.
- **Eight broker authentication event types begin emitting.** `register`, `deregister`, `join`,
  `rotate`, `revoke`, `link`, `unlink` and **`auth_failure`** produce log records again. They have
  emitted **nothing at all** until now, so **plan for the added log volume** — anything consuming
  these events necessarily treats their absence as normal today, because absence is all there has
  ever been. Administrative events log at INFO and any failure logs at WARN. The one high-volume
  type, per-request `auth_success`, now logs at DEBUG and so stays out of normal operation; that
  was the volume problem behind the original silencing, and it is fixed at the level of the one
  noisy type rather than by muting the other eight. Downstream consumers have not been surveyed.
- **`PATCH /agents/{id}` with a non-existent service account ID now returns 400 instead of 404.** The
  prior 404 distinguished existence from reachability, making the endpoint an existence oracle for
  SA IDs. Agent create already returned 400 for both cases but distinguished them by message; both
  paths now give the single answer `GCP service account not available in this project`, matching the
  project-default settings path, which has behaved this way since it was written. **Anything that
  branches on 404 from this endpoint to mean "no such service account" will need updating** — it
  now sees 400, the same as every other rejected assignment.
- **Service-account assignment decisions are now audited.** Assigning a GCP service account to an
  agent (create, PATCH, project default) or to a lifecycle hook's execution identity emits a record
  on **both allow and deny**. While the `actAs` check is inert (see below), allow records carry
  mechanism `check-disabled`, which is what distinguishes "allowed because IAM said so" from
  "allowed because nobody asked". Records are log lines, not a queryable store.

### Known limitations
- **GCP `iam.serviceAccounts.actAs` checking is inert on service-account assignment.** Assigning a
  GCP service account to an agent (create and PATCH) and setting a lifecycle hook's execution
  identity are gated by Scion's own authorization policy, but **no caller is checked for
  `iam.serviceAccounts.actAs` on the target account**. The Hub logs a warning at startup for each
  affected surface. Note that the lifecycle-hook surface has no second policy layer to fall back
  on: any caller who may write a hook may run it as any in-scope verified service account.

  Turning the check on requires **two switches, not one**, and setting only the first has no
  effect:
  1. the **mode** (`saAssignCheckMode` / `hookIdentityCheckMode`), and
  2. the **checker** (`saAssignChecker` / `hookIdentityChecker`), which is currently always the
     disabled checker.

  In `enforce` mode with the disabled checker still installed, assignment is still allowed —
  `enforce` alone only changes what happens when the Hub has no GCP token generator, in which case
  it refuses. Neither switch is settable from configuration in this release; the GCP prober that
  would answer the question for real does not exist yet. The gate itself is wired end to end and
  has tests that drive each surface over HTTP with a real denying checker installed.

---

## v0.1 — Initial Release

Multi harness agent orchestrator

### Features
- Project scaffolding generated with appteam
- Multi-agent team structure configured
- Development pipeline and workflow established

### Team
- SWE-1: General Engineer 1
- SWE-2: General Engineer 2
- SWE-3: General Engineer 3
- SWE-4: General Engineer 4
- SWE-5: General Engineer 5
- SWE-Test: Automated testing
- SWE-QA: E2E testing & QA
- Platform Engineer: Infrastructure & deployment
- Reviewer: Code review & quality
