# Release Notes (2026-09-09)

GCP identity on sandbox runtimes unblocked end-to-end, broker lifecycle hardened with heartbeat timeouts and clean unregistration, and a hub-level default broker setting added for multi-broker deployments.

## 🚀 Features
* **Hub-level default runtime broker (#1521):** Adds `DefaultRuntimeBroker` to `AgentDefaultsSettings` as step 2.5 in the broker resolution cascade (between project default and auto-select). Admin UI with broker dropdown, agent-create UI fallback, and 7 new cascade tests.
* **Broker unregister button (#1515):** Admin-gated Unregister button with confirmation dialog on the broker detail page. Backend cleans up HMAC secrets and join tokens; `DeleteBrokerSecret` and `DeleteJoinToken` registered in the authzop catalog.

## 🐛 Fixes
* **GCP passthrough → assign translation on sandbox runtimes (#1509):** gVisor sandboxes cannot reach the GCE metadata server, so passthrough mode produces no credentials on `cloudrun-sandbox` runtimes. Hub now translates passthrough to assign using the broker's host SA at create and PATCH time — downstream JWT scope, `resolvedEnv`, and `gcp-token` endpoint work automatically.
* **`GOOGLE_CLOUD_LOCATION` propagation to sandbox agents (#1517):** Three compounding bugs prevented Gemini CLI vertex-ai authentication: `ResolveAuth` collapsed `GOOGLE_CLOUD_LOCATION` into `GoogleCloudRegion` without reconstituting it, `cloudrun_sandbox_runtime.go` never called `applyResolvedAuth()`, and the Gemini CLI `settings.json` `allowedEnvironmentVariables` was missing GCP credential vars.
* **Auth autodetect for passthrough-translated agents (#1514):** After #1509 translates passthrough to assign, `hasRequiredAuthCredentials()` still did not recognize the translated agent as having a GCP SA. Added `agentHasGCPIdentityAssigned()` helper mirroring broker logic.
* **Claude harness invalid settings blocking startup (#1513):** Wildcard `*` in `permissions.allow` and an invalid `ModelResponse` hook event in Claude harness `settings.json` caused Claude Code 2.1.266 to present an interactive Settings Warning prompt, blocking headless agent startup indefinitely.
* **Broker heartbeat timeout scheduler (#1518):** Adds a 5-minute-cycle recurring scheduler that marks stale brokers offline when the WebSocket disconnect event fails to fire (hub crash, network partition). Mirrors the existing `agent-heartbeat-timeout` pattern.
* **Agent delete hang on stale broker (#1520):** Phase-aware delete skips broker dispatch for created-phase agents that were never provisioned. Web UI adds a force-delete fallback confirmation dialog on 502/503, and delete timeout reduced from 90s to 15s.
* **Web chat store initialization (#1524):** Chat and attachment store initialization was nested inside the `MessageBroker.Enabled` gate, so single-node Cloud Run instances showed "No spaces available." Moved initialization outside the gate.
* **Restart button 502 error (#1519):** Admin restart button now uses fire-and-poll with exponential backoff on `/healthz` instead of fire-and-check, showing a loading spinner and success toast rather than a misleading 502 error.
* **Broker detail build fix (#1522):** Removed the extra closing brace in `broker-detail.ts` (introduced by #1515) that broke the vite build.

## 📖 Docs
* **Nightly doc update (#1512):** Added Python package index proxy section to `custom-images.md` (mirroring npm pattern from #1488) and `project:template:write` scope to the Full agent role list in `security.md`.
