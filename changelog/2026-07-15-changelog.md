# Release Notes (2026-07-15)

The busiest day yet with 36 commits: gcloud ADC auto-injection shipped with user opt-in gating, OpenCode gained auth file capture and model flag support, harness image state got workstation-mode improvements, and a wave of plugin lifecycle and integration UI fixes landed.

## 🚀 Features
* **[Server]:** Auto-configure hub endpoint in workstation mode (#744).
* **[Runtime Broker]:** Gate gcloud ADC auto-injection behind user opt-in setting and onboarding wizard — prevents unexpected credential exposure (#757).
* **[Settings]:** Moved `auto_inject_gcloud_adc` to top-level settings with profile page toggle (#760).
* **[OpenCode]:** Auth.json file secret capture, no-auth login command, and `capture_auth` fix (#737, #743).
* **[OpenCode]:** Pass `--model` flag to OpenCode when model is configured (#742).
* **[Hub]:** Re-parse image from `config.yaml` when harness-config file is saved or uploaded (#761).
* **[Hub]:** Show local image state when all brokers are proxy-typed in workstation mode (#755).
* **[Hub/Web]:** Show full image state in harness detail for workstation/podman mode (#738).

## 🐛 Fixes
* **[Hub]:** Accept registered-but-not-active plugins in `HasPlugin` and `ListPlugins` (#770).
* **[Hub]:** Respect `alternative_env_keys` in required_files credential check for gcloud-adc (#751, #752).
* **[Hub]:** Show installed-but-unconfigured integrations by reading settings.yaml (#756).
* **[Hub]:** Exclude settings-registered plugins from available list; handle unconfigured plugin GET (#758, #759).
* **[Hub]:** Skip build when plugin binary is already on `$PATH` (#735).
* **[Hub]:** Treat `LoadOne` failure during PATH-binary install as non-fatal (#753).
* **[Hub]:** Load PATH-binary plugin into manager so it appears in installed list (#740).
* **[Hub]:** Skip notification lookup when `agent_id` is empty (avoids invalid UUID error) (#750).
* **[Hub]:** Retry group creation without owner on FK violation (#749).
* **[Telegram]:** Admin UI config fields and bot token UX fixes (#764, #767).
* **[Web]:** Show secrets section when `has_secrets` is null (#769).
* **[Web]:** Sort harness configs alphabetically by `displayName` (#736).
* **[Web]:** Strip `git+` prefix from harness config source URL link (#739).
* **[Config]:** Detect v1-shaped runtime fields when `schema_version` is missing (#734).
* **[Onboarding]:** Derive image pull total from server events, not harness slug count (#741).
* **[OpenCode]:** Fix `capture_auth` ordering to prevent "already exists" message (#747).
* **[Ent]:** Remove stale `discordpendinglink` references from generated ent client (#745).
* **[CI]:** Remove illegal backslash from `harness_config_handlers.go` (#746, #748), apply `gofmt` after #761 (#763), address review feedback on #760 (#765).

## 📖 Docs
* **[Docs]:** Rework templates page around roles and skills (#768).
* **[Docs]:** Update harness auth for container-script provisioning workflow (#766).
* **[Docs]:** Skill Bank, harness lifecycle, and Homebrew-first install (#762).
