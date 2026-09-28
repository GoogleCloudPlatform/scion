# Release Notes (2026-09-13)

Broad stabilization sweep — credential-leak prevention in clone and image-pull paths, metrics dashboard admin-gated, secret promotion propagates `AllowProgeny`, and several broker/agent fixes from the miller79 backlog.

## 🔒 Security
* **Metrics dashboard admin gate (#1581):** Metrics nav was reachable by non-admin users despite requiring admin-only API. Moved to `ADMIN_SCOPEABLE_ITEMS`, added `hub.metrics.read` to permission maps, and registered the project-scoped subclass so it renders correctly without the admin gate.
* **Credential leak on clone failure (#1577):** Git clone credentials persisted in `.git/config` when workspace init failed. Deferred `.git/` cleanup after init prevents credential-bearing URLs from remaining on disk.
* **PullImage output and credential safety (#1582):** `PullImage` used `runInteractiveCommand`, losing diagnostic output on failure. Switched to `runSimpleCommand` with per-site error enrichment scoped to Docker, Podman, and AppleContainer call sites where no secrets appear in argv. Credential leak regression test verified passing.

## 🐛 Fixes
* **AllowProgeny propagation on secret promotion (#1580):** The `setEnvVar` secret promotion path did not propagate the `AllowProgeny` flag, breaking progeny inheritance for promoted secrets. Added clarifying comments on all 3 secret validation blocks referencing design doc section 5.7.
* **Telemetry cloud export gating (#1578):** GCP cloud export was attempted without a `ProjectID`, metric re-buffer loop was unbounded on consecutive failures. Now gates export on `ProjectID`, bounds the re-buffer loop with a consecutive failure counter, adds structured gRPC error classification, and includes actionable startup warnings.
* **Stale project marker on recreation (#1573):** Recreating a git-backed project with the same name left a stale on-disk marker pointing to a deleted UUID. `buildStartContext` now detects and overwrites stale markers using external config dir existence as the staleness heuristic.
* **isGit preservation for skill injection (#1570):** `inject_when: git_workspace` could never match in clone-per-agent workspace because `isGit` was forced `false` inside containers. Now preserves the original value before the container override and uses it for skill injection context.
* **Chat markdown HTML truncation (#1565):** Angle-bracket text (e.g. `<template>`) was interpreted as real HTML elements by the markdown renderer, causing message truncation. Added a custom marked renderer hook that entity-escapes raw HTML tokens.
* **ADO clone URL `.git` suffix (#1584):** Azure DevOps repository URLs with a `.git` suffix broke clone operations. Strips the trailing suffix in the ADO provider.
* **Default `--registry` from env var (#1576):** `build-images.sh` without `--registry` produced unusable images. Now defaults `REGISTRY` from `SCION_IMAGE_REGISTRY` env var with a warning when neither is set.
* **Harness-config resolution error messages (#1575):** `FindHarnessConfigDir` errors now list the searched directories, and `hydrateHarnessConfig` logs a WARN when hydration skips unstamped dispatch.

## 📖 Docs
* **BYO-TLS for internal GCE deployments (#1572):** Added section to `hub-setup-gce.md` documenting internal deployment path for hosts without public IP — `SCION_SERVER_BASE_URL` configuration, skippable scripts, and Caddy config with custom certs.
