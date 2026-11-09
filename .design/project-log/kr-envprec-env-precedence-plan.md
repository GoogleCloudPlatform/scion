# Env precedence parity: Docker, Kubernetes, Cloud Run sandbox

**Date:** 2026-11-07
**Issue:** ptone/scion#7382

## Problem

An agent container env var can come from several sources: resolved auth, the harness env (GetEnv, then GetTelemetryEnv), the merged config env (`RunConfig.Env`), env-type secrets, runtime keys, and the template fallback. Each runtime orders them itself, so a name set by two sources can resolve differently per runtime.

## Findings (upstream main 2955993c)

- **Docker** (`buildCommonRunArgs`, `pkg/runtime/common.go`; the last `-e` wins): resolved auth < harness env < runtime keys < `cfg.Env` < env-type secrets (skipped only on a `cfg.Env` or reserved name) < template fallback. Empty values are skipped.
- **Cloud Run Instances** follows the Docker order and documents it (`buildCloudRunInstance`).
- **Kubernetes** (`buildPod`, `pkg/runtime/k8s_runtime.go`; duplicates collapse to the last entry):
  - Resolved auth is appended after `cfg.Env` and the secrets, so it wins. Empty auth values are kept, and the map order varies between starts.
  - An empty telemetry value blanks a GetEnv value.
  - A harness env name blocks an env-type secret of the same name.
  - `TestKubernetesRuntime_BuildPod_DedupesEnv` pins resolved auth winning.
- **Cloud Run sandbox** (`envFor` and `Run`, `pkg/runtime/cloudrun_sandbox_runtime.go`):
  - The harness env overrides `cfg.Env`, and resolved auth overrides both.
  - There is no telemetry env, and empty values are kept.
- **Docker only:** `cfg.Env` can override runtime keys (`SCION_HOST_UID`/`GID`, `SCION_PROJECT`, `SCION_PROJECT_ID`, `SCION_WORKSPACE_BACKEND`). On the other runtimes the runtime value wins.
- The pre-runtime merge (`buildAgentEnv` in `pkg/agent/run.go`, plus the broker start context) is shared by all runtimes. Every divergence is in the runtime layer.

## Slice (one Kubernetes change in `buildPod`)

1. Emit the resolved-auth env first, through the existing `resolvedAuthEnvEntries` helper (sorted, empty values skipped).
2. Apply harness and telemetry env through the existing `harnessEnvEntries` helper, so empty telemetry values are skipped.
3. Build the secret collision set from `SCION_START_CMD` and the `cfg.Env` names only. An env-type secret then wins over resolved auth and over a harness env name, while `SCION_START_CMD` stays protected.

Result: Kubernetes follows the Docker order except for the runtime keys, which are kept. Tests pin the order, including the flipped `DedupesEnv` expectation. The docs change is a System A subsection (A7) in `settings-precedence.md`.

## Out of scope

- The Docker-only runtime-key difference: kept, and listed in the PR text.
- Sandbox ordering: deferred, because it shares the resolved-auth and secret helpers with other Cloud Run work.
- The harness agent-home contract on remote runtimes: a harness-contract decision.
- Cloud Run bare env names: no code sets them yet.

## Lesson for precedence tests

Prove the collision exists in the test setup. A harness value that resolves to empty is silently dropped, so a test built on it checks nothing. The buildPod tests assert each collision premise (`requirePremise`) before they assert the winner. The secret-over-harness test uses `HARNESS_AGENT_NAME`, because `HARNESS_HOME` is empty when the test config sets no `HomeDir`.

## Test environment notes

- A unix-socket test needs a short `TMPDIR`: unix socket paths must stay under about 107 bytes.
- A file-mode test assumes `umask 022`, as CI uses.
