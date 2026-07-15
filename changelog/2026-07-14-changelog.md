# Release Notes (2026-07-14)

A major HA and admin settings day: the hub admin settings system shipped with seed/managed model and layer-aware UI, env-gather finalize was rewritten for stateless HA replay, the broker gained runtime hot-swap on container engine change, and workspace skill overlay was removed in favor of embedded platform skills.

## 🚀 Features
* **[Hub]:** Hub admin settings — seed/managed model with `SCION_SEED_*` env var provider, bootstrap merge (SEED → yaml → SERVER layering), deprecation detection, and layer-aware admin UI showing which settings are seeded vs managed (#697).
* **[Hub]:** Auto-detect gcloud ADC credentials in workstation mode (#719).
* **[Web]:** Copy-to-clipboard for service account emails on project settings page (#722).

## 🐛 Fixes
* **[Broker]:** Replay-based stateless env-gather finalize for HA — rewrites `DispatchFinalizeEnv` to rebuild the full create request via `buildCreateRequest` and dispatch through `CreateAgentWithGather`, fixing 404s when finalize-env routes to a different broker replica (#721).
* **[Broker]:** Reload co-located broker runtime on container engine change — `SwapRuntime` swaps the runtime client and agent manager in-place when the onboarding wizard or PUT handler changes the engine, fixing CLI/broker mismatch (#714).
* **[Runtime]:** Return user-friendly 409 error when agent name already in use — detected at runtime, broker, and hub layers with actionable message. Works for Docker, Podman, and Apple Container (#716).
* **[Hub]:** Surface plugin install/reconfigure errors in HTTP response with sanitized messages (#718).
* **[Server]:** Skip container runtime probe in hosted mode (#726).
* **[Store]:** Gate `skipExistingRelations` hook to Postgres only (#727).
* **[Web]:** Use harness config dropdown on server config page (#715).
* **[Web]:** Use password-style font in secret textarea (#713).
* **[Web]:** Agent create form fix (#728).
* **[Web]:** Syntax highlighting for JSON and YAML in workspace file viewer (#724).
* **[Provision]:** Prevent root-owned `__pycache__` from persisting after delete (#723).
* **[CI]:** `gofmt` remaining files on main (#732), simplify embedded field selectors for staticcheck QF1008 (#730), update stale git pull test assertions (#729).

## 🔄 Refactor
* **[Skills]:** Removed workspace skill overlay from provisioning pipeline — redundant with embedded platform skills. Deleted `injectWorkspaceSkills()`, root `skills/` directory, and associated tests (#711).
* **[Skills]:** Added deprecated field warnings to `team-creation` skill (#720) and skill bank URI references (#717).

## 📖 Docs
* **[Docs]:** Added GCP Skill Registry tutorial (#712).

## 🔧 Chores
* **[Deps]:** Bumped esbuild, @astrojs/starlight, and astro in docs-site (#731).
