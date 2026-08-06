# Release Notes (2026-08-05)

A targeted fix day resolving a P0 broker plugin restart crash, correcting the antigravity ADC auth design, and cleaning up a UI inconsistency.

## 🐛 Fixes
* **[Plugin]:** Resolve RPC bootstrap race condition on broker restart — fixes Discord plugin death after web UI Restart/Update. Three interacting bugs: `MuxBroker` double-Dial stall on reconfigure (affects all broker plugins), session replacement without gateway close, and bootstrap goroutine accumulation. Observed on 3 hubs in 48 hours (#1051).
* **[Antigravity]:** Rework ADC auth under `vertex-ai` type instead of a standalone `adc` type — fixes P0 CI break (`TestBundleInstall_Antigravity` schema validation failure) from PR #1047. `vertex-ai` now uses `gcloud-adc` file with `AGY_ADC_AUTH=true` (matching the claude/hermes pattern), `oauth-token` retains the keyring flow (#1053).
* **[Web]:** Hide graph-view toggle button on the skills page — no graph view exists for skills (#1052).
