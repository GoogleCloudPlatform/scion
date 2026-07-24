# Release Notes (2026-07-20)

Discord multi-server support shipped end-to-end: multi-guild command registration with concurrent processing, guild-removal cleanup with guild_name tracking, admin UI for guild_ids and bot invite links, and observe mode identity fixes. Harness model resolution was fixed across OpenCode, Hermes, and Gemini CLI, and a mandatory instruction preamble injection mechanism was added to the provisioning pipeline.

## 🚀 Features
* **[Discord]:** Multi-guild command registration — `Config.GuildIDs` replaces singular `GuildID`, with concurrent registration across guilds, backward-compat fallback, and updated env keys (#819).
* **[Discord]:** Guild-removal cleanup and `guild_name` tracking — `handleGuildDelete` deactivates links when the bot is removed (with outage guard), additive schema migration adds `guild_name` column populated from session cache (#816).
* **[Web]:** Expose `guild_ids` config in Discord admin integrations UI with comma-separated input and "Global — all servers" placeholder (#822).
* **[Web]:** Bot invite link button in Discord admin UI — constructs OAuth2 authorize URL with permissions bitmask when Application ID is populated (#826).
* **[Web]:** Wire Help button to open docs site in new tab (#827).
* **[Provisioning]:** Mandatory instruction preamble injection — embedded `mandatory_boilerplate/` FS prepended to every provisioned agent's instructions regardless of template (#825).

## 🐛 Fixes
* **[Harness]:** Fix model resolution in OpenCode and Hermes — `ctx.model_resolution` is always empty because `ProvisionManifest` has no `model_resolution` field; fall back to `SCION_MODEL` env var (#828, #829).
* **[Harness]:** Gemini CLI — inject `GEMINI_SYSTEM_MD` env var for system prompt pickup, add single-letter model alias mappings (S/M/L), and perform fallback alias resolution in `provision.py` (#830).
* **[Discord]:** Observe mode sender identity — derive `senderSlug` from `msg.Sender` instead of topic agent, add embed styling, remove duplicate text content in relay (#818).
* **[Discord]:** Run guild command registration in goroutine to avoid blocking the event loop (#820).
* **[Discord]:** `gofmt` cleanup for `extras/scion-discord` to unblock main CI (#823).
* **[CLI]:** Remove stale transport-audience-mismatch test case — decoupled by #814, test was not updated (#824).

## 📖 Docs
* **[Discord]:** Multi-server setup guide — `guild_ids` config, trust model, invite flow, guild removal behavior, and operational notes (#817).
