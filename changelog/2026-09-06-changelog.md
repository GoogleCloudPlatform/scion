# Release Notes (2026-09-06)

Heavy fix day focused on the single-node Cloud Run tier and credential management — cold-start super-admin bootstrap, GCP identity passthrough, `as_needed` secret autodetect, and a GCP Secret Manager error mapping fix that had caused ~15h Discord downtime.

## 🚀 Features
* **Internal npm registry for image builds (#1476):** `NPM_REGISTRY` build arg and `NPM_CONFIG_FILE` BuildKit secret enable image builds behind corporate proxies where registry.npmjs.org is blocked. Backward-compatible — defaults to public registry.

## 🐛 Fixes
* **GCP Secret Manager NotFound mapping (#1478):** gRPC `NotFound` in `GCPBackend.Get()` was wrapped as a generic error instead of `store.ErrNotFound`, breaking `MigratePluginSecrets` which skips migration on unexpected errors — caused ~15h of Discord downtime on scion-sagan.
* **Cold-start super-admin RoleBinding (#1480):** `ReconcileSuperAdminBindings` runs at startup before any users exist, so the first admin got `User.Role=admin` but no system-scoped super-admin RoleBinding. Fixed by adding `ensureSuperAdminBinding()` at all three admin-role assignment paths (create, invite-to-active, promotion).
* **`as_needed` secret keys for broker autodetect (#1483):** Hub-level `as_needed` secrets (e.g. `GEMINI_API_KEY`) were invisible to the broker autodetect when the harness default_type didn't require them. Hub now passes `AvailableAsNeededKeys` so the broker can select the correct auth type.
* **Host SA auto-detection from metadata (#1482):** Co-located broker registration never set `gcpHostServiceAccountEmail`, breaking GCP identity passthrough on single-node tier. Now auto-detects from GCE metadata server; validator widened to accept `@developer.gserviceaccount.com` (default Compute Engine SA).
* **Cloud Run sandbox defaults on single-node tier (#1481):** `GetDefaultSettingsDataYAML()` always returned the workstation template, causing "remote (kubernetes)" to appear as the only runtime profile. Now returns the cloudrun-sandbox template when running on that tier.
* **Antigravity effort flag and thinking tiers (#1479):** Harness crashed on startup with thinking level set — wrong CLI flag (`--thinking-level` → `--effort`) and wrong tier mapping (mixed-case → lowercase matching Antigravity's expectations).
* **Authenticated clone URLs for Azure DevOps (#1475):** `CloneSharedWorkspace` injected credentials by substituting on `https://`, which broke on Azure DevOps URLs containing userinfo. Now uses `net/url` parsing to properly set Userinfo.
* **Chat sidebar terminal link (#1474):** Terminal link in chat members sidebar no longer opens a new tab — navigates in-app, matching the visual distinction between terminal and pop-out icons.
* **Demo provision monitoring role (#1473):** Added `roles/monitoring.viewer` to `gce-demo-provision.sh` — the service account could write metrics but not read them back, breaking the metrics dashboard.
