# Release Notes (2026-07-01)

A massive infrastructure day: the harness system was refactored from compiled builtins to a bundled resource catalog with directory-based provisioning, a Gemini CLI harness shipped, chat integration admin landed across API and UI phases, the A2A bridge adopted the official SDK, and HA reliability received targeted fixes.

## 🚀 Features
* **[Harness]:** Normalized Claude to directory-based provisioning — moves Claude's harness config from compiled Go code to a standalone `harnesses/claude/` directory, completing the pattern established by PR #279's `provision.py` migration (#548).
* **[Resources]:** Introduced bundled resource catalog for Templates and Harness-configs — embedded resources are now declared in a catalog and promoted through a `ResourceSource` interface with `BootstrapSource` for the hosted startup path and `MaterializeBundledResources` for workstation local seeding (#549, #550, #551, #552).
* **[Harness]:** Gemini CLI container-script harness bundle — full provisioning model with API key/OAuth/Vertex AI auth detection, model aliases, `capture_auth.py`, Dockerfile, and Cloud Build config. Migrates Gemini from the builtin harness to the same pattern as Claude (#563).
* **[Build]:** Refocused image builds on base image and harness catalog — build pipeline now produces a single `scion-base` image plus per-harness images from the catalog (#561).
* **[Chat Admin]:** Chat integration admin API endpoints (Phase 2) — CRUD operations for managing integration plugins via the Hub API (#543).
* **[Chat Admin]:** Chat integration admin UI (Phase 3) — new `/admin/integrations` page with list/detail views, config forms, secrets management, and restart controls (#556).
* **[A2A Bridge]:** Adopted the official `a2a-go` SDK for protocol handling — replaces hand-rolled JSON-RPC with spec-compliant server, `ScionExecutor` bridges SDK events to Scion Hub routing. Preserves auth, metrics, and multi-project routing (#362).
* **[Hub]:** HA robustness improvements — added scheduler jitter (0-30s) and increased non-critical task intervals from 1 to 5 minutes to prevent DB connection thundering herd. Idempotent broker secret for co-located mode survives Cloud Run restarts (#555).
* **[Hub]:** Storage validation, repair, and CLI `validate` commands for diagnosing and fixing storage inconsistencies (#553).

## 🐛 Fixes
* **[Agent]:** Exclude soft-deleted agents from slug lookup and clean stale directories (#547).
* **[Message]:** Reject `--attach` in local (no-Hub) mode instead of silently dropping attachments — also rejects `--attach` combined with `--in`/`--at` since scheduled sends don't carry attachments.
* **[Build]:** Skip image registry rewrite for fully qualified references — prevents double-prefixing when images already include a registry hostname (#566).
* **[Web]:** Dir-browser UX improvements and safety fixes (#559).
* **[Server]:** Improved server lifecycle reliability (#558).
* **[Config]:** Updated `allow_container_script_harnesses` default to `true` in schema (#557).
* **[Config]:** Misc correctness fixes — Makefile, `InitProject`, GitHub URL import (#560).
* **[Web]:** Added plug icon to bundle and hidden `bot_id` from config UI (#562).
* **[Harness]:** Removed legacy seeding dead code and fixed logger subsystem (#554).

## 📖 Docs
* **[Build]:** Warned that `go install` produces a blank web UI and fixed build-from-clone steps (#565).

## 🔧 Chores
* **[Deps]:** Bumped `golang.org/x/net` in A2A bridge (#569).
