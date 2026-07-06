# Release Notes (2026-07-05)

The largest single commit in recent history landed: Phase 5 HA (Mode 3) support — a complete high-availability architecture for chat integrations with gRPC broker protocol, advisory lock failover, standalone Discord deployment, and an integration runtime library. Platform skills also became embedded in the binary.

## 🚀 Features
* **[HA]:** Phase 5 HA (Mode 3) support — a sweeping ~26,600-line change across 92 files delivering:
  - **gRPC broker protocol** (`proto/broker/v1/`) for cross-process integration communication with adapter, factory, and server
  - **Integration runtime library** (`pkg/integration/runtime/`) with config resolution (DB > env > YAML layering), admin signal listener, update signal handling with status write-back, and schema retry with backoff
  - **Standalone Discord entry point** with `--standalone` flag, Postgres-backed `discord_pending_links`, advisory lock loop with takeover delay and lock-loss detection, gRPC health service, and graceful shutdown ordering
  - **Transactional NOTIFY** for admin signals, cross-integration ID leakage guards, `updated_by` tracking
  - **PostgresConfigProvider** for HA config persistence replacing YAML-only storage
  - Multi-stage Dockerfile and comprehensive standalone deployment documentation (#608)
* **[Agent]:** Platform skills embedded in binary via `go:embed` and injected into all agents at provisioning time — `scion`, `scion-cli-operations`, `scion-messaging`, `agent-status-signals`, `team-creation`, and `git-sandbox` (conditional on `isGit=true`). Runs after template skills but before workspace skills (#610).

## 🐛 Fixes
* **[Hub]:** Check hub-scoped env vars in `hasAnyKey` credential check — child agents whose owner is a parent agent (not a human user) can now find credentials at hub scope (#611).
* **[Claude]:** Fixed env overlay variable resolution in `provision.py` — values now read from `auth_candidates.json` or staged secrets instead of `os.environ`, with `_resolve()` fallback to shell references (#607).
* **[Claude]:** Expanded env var references in Vertex AI env overlay — replaced literal `'${VAR_NAME}'` strings with actual `os.environ.get()` calls across all auth branches (#606).
