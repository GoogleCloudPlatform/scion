# Release Notes (2026-07-12)

A heavy stabilization day targeting hosted/HA deployment reliability: broker runtime detection, plugin lifecycle, and control channel fixes addressed production issues in Cloud Run and multi-broker setups. Discord routing and Telegram attachments also received critical fixes.

## 🚀 Features
* **[Harness]:** Per-broker container image state for harness-config detail page — shows image availability status across each connected broker (#695).

## 🐛 Fixes
* **[Broker]:** Made control channel dispatch async to prevent head-of-line blocking — a slow dispatch no longer blocks all subsequent agent operations (#694).
* **[Broker]:** Skip Docker runtime initialization when docker binary is unavailable — Cloud Run deployments defaulted to Docker even without the binary, causing repeated `docker ps` heartbeat errors. Now falls back to cloudrun runtime when `K_SERVICE` is set (#690).
* **[Broker]:** Filter profiles by detected runtime, not hosted flag — fixes starter-hub VMs running both hub and broker with Docker available. Adds fallback default profile when all configured profiles are filtered out (#686).
* **[Hub]:** Apply `image_registry` when dispatching agents to remote brokers (#698).
* **[Hub]:** Reconstruct hub wiring credentials from live sources on reconfigure — `getPluginHubCreds()` rebuilds `hub_url`, `broker_id`, `hmac_key`, `project_slug_map` from authoritative sources instead of relying on the empty plugin manager cache (#691).
* **[Hub]:** Restore wiring keys and eventbus spoke after plugin restart (#693).
* **[Hub]:** Snapshot runtime wiring keys before plugin restart in `reconfigureIntegration` (#688).
* **[Hub]:** Skip config file creation if file already exists in `handleInstallIntegration` (#692).
* **[Discord]:** Fixed @mention routing, filter system messages, inherit parent channel config (#696).
* **[Telegram]:** Pass `thread-id` to attachment send API calls (#689).
* **[Server]:** Skip container runtime probe in hosted mode (#683).
* **[Config]:** Add camelCase koanf field support for `SCION_SERVER_` env vars (#684).
* **[Store]:** Make Ent `Schema.Create` idempotent on Postgres — skip `42P07` (duplicate table) errors (#685).
* **[Harness]:** Apply `system_prompt_flag` in `ContainerScriptHarness.GetCommand` (#682).
* **[Harness]:** Updated Claude harness `interrupt_sequence` in config.yaml.
* **[Web]:** Fixed agent create form showing legacy harness list instead of Hub configs (#687).
* **[Provision]:** Prevent root-owned `__pycache__` from persisting after agent delete (#681).
