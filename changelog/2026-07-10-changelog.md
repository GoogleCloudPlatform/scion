# Release Notes (2026-07-10)

HA multi-tenant isolation deepened with GCS path namespacing by hub ID, a new `hub_name` operational setting, and broker type labels. The web UI gained syntax highlighting for workspace files and the brokers page was cleaned up.

## 🚀 Features
* **[Storage]:** Namespace GCS paths with hub ID for HA multi-tenant isolation — all GCS paths now follow `gs://{bucket}/hubs/{hub-id}/{resource-kind}/{scope}/{slug}/`, eliminating cross-hub manifest hash desync that caused agent dispatch failures (#670).
* **[HA]:** Introduced `hub_name` as a Layer-1 operational setting, replacing hostname-based hub identity — `hub_id` relaxed to accept any string slug (not just hex), `hub_name` provides human-readable display name for HA deployments (#667).
* **[Brokers]:** Broker type labels and human-readable names — co-located brokers display as "Hosted Broker" instead of `os.Hostname()`, with `scion.io/broker-type: hosted|external` labels and type badges in the web UI (#666).
* **[Web]:** Syntax highlighting for JSON and YAML in workspace file viewer — routed through CodeMirror in read-only mode instead of opening in a raw browser tab (#665).

## 🐛 Fixes
* **[Broker]:** Co-located heartbeat now respects control channel state (#672).
* **[Gemini CLI]:** Updated model aliases (`medium` → `gemini-3.5-flash`, `large` → `gemini-3.1-pro-preview`) and fixed `auth.selectedType` not written in no-auth path (#671).
* **[Gemini CLI]:** Updated image name in config.
* **[UI]:** Filter message brokers from `/brokers` page, showing only runtime brokers (#669).
* **[Plugin]:** Treat `ErrNotFound` as safe-to-create in `migrateInlineSecrets` — secrets that don't exist yet are now created instead of skipped. Also inject secrets into merged config before `LoadAll` calls `Configure` (#668).
