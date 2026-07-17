# Release Notes (2026-07-16)

Telegram V2 became the default broker, the message broker subsystem now auto-enables when plugins are configured, and a cluster of plugin config lifecycle fixes resolved the chicken-and-egg deadlock for fresh installs. Copilot received home-dir path fixes and hook support.

## 🚀 Features
* **[Telegram]:** V2 broker is now the default — legacy V1 available via `SCION_TELEGRAM_V1=1` env var (#774).
* **[Server]:** Auto-enable message broker when broker plugins are configured — prevents silent failures where bots appear configured but never start (#775).

## 🐛 Fixes
* **[Hub]:** `PUT /config` falls back to `settings.yaml` for installed-but-unloaded plugins — resolves the deadlock where config needed to load the plugin could never be saved on fresh installs. After saving, the plugin is activated via `LoadOne` with full resolved config (#772).
* **[Hub]:** Store `config_file` path immutably so `PUT /config` can always find it (#771).
* **[Hub]:** Replay fanout subscriptions onto spokes added via `AddSpoke` — fixes dead inbound messages after `PUT /config` activates a plugin (#773).
* **[Broker]:** Enumerate configured profiles in broker info endpoint (#783).
* **[Broker]:** Skip Docker heartbeat when docker binary is unavailable (#781).
* **[Config]:** Add `cloudrun` to V1 settings schema runtime type validation (#782).
* **[Copilot]:** Home-dir paths, hook support, and instructions improvements — instructions now write to `~/.github/` instead of workspace, added hook support via `~/.copilot/hooks/scion.json` (#777).
* **[Copilot]:** Fixed duplicate `hooks` key in `config.yaml` (#778) and removed top-level hooks metadata block (#780).

## 📖 Docs
* **[Docs]:** Added harness-config authoring guide (#779).
* **[Docs]:** Telegram tutorial (#776).
* **[README]:** Updated with Scion applications and poster image.
