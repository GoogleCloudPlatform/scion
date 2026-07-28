# Release Notes — Scion

## Unreleased

### Fixed
- **K8s attach pod name resolution** — `scion attach` now uses the actual grove-prefixed pod name (e.g., `sciontest--hello`) instead of the bare agent name, fixing GKE Warden `autogke-no-pod-connect-limitation` errors
- **K8s attach su password prompt** — `scion attach` no longer prompts for a password on GKE Autopilot pods that run as non-root with `allowPrivilegeEscalation: false`

### Added
- **Resolved project settings endpoint** — `GET /api/v1/projects/{projectId}/settings/resolved` reports every project setting alongside whether a hub-level default exists. Requires `ActionRead` on the project (not admin-gated), so project owners can see that hub defaults exist. Despite the name it does **not** return an effective value: precedence is owned by the code that applies it, and a second implementation here would drift silently. `hubDefault` is tri-state (`present`/`absent`/`unknown`) — `unknown` means the hub could not determine it, and must not be rendered as "no hub default". See the [reference page](https://googlecloudplatform.github.io/scion/reference/project-settings-resolved/).
- All container images built and published to Artifact Registry (core-base, scion-base, scion-claude, scion-gemini, scion-opencode, scion-codex)

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
