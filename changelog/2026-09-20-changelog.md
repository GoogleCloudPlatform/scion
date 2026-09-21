# Release Notes (2026-09-20)

Bounded GCP observability and native harness telemetry lands, cross-project messaging UAT defects fixed, and pgx UUID type registration resolves agent-creates-agent Postgres failures.

## 🚀 Features
* **Bounded GCP observability and native harness telemetry (#1792):** Five-phase metrics work adding bounded OTLP admission/delivery, GCP metric identity and cadence, native harness telemetry configuration, privacy controls, diagnostics, tests, and runbook. Claude logs-first route accepted and deployed live.

## 🐛 Fixes
* **Cross-project messaging UAT defects (#1783):** Five integration defects fixed from live testing — CLI addressing, UI interagent visibility, interagent authz (`ActionAttach`), hub-off read guard (detail/list/resolve), and conversation send dispatch with canonical peer derivation.
* **pgx UUID type registration (#1787):** `google/uuid.UUID` was encoding as text (OID 25) instead of native uuid (OID 2950) with pgx v5, causing `SQLSTATE 42883 "operator does not exist: uuid = text"` on the agent-creates-agent delegation-ceiling check. Registered with pgx type system; Postgres integration test added.
* **SA verified state on project clone (#1788):** Preserve service account verified state and remap the default SA annotation during project clone.
* **Nightly release workflow (#1790, #1793):** Nightly builds weren't triggering the release workflow due to `GITHUB_TOKEN` limitation — now invokes `build-release` directly via `workflow_call`. Added repository guard to prevent nightly workflow from running on forks.
* **Project settings page order (#1789):** Members and Resources now appear immediately after Configuration (positions 2-3), before Messaging Policy and other sections.

## 📖 Docs
* **Messaging authorization reference (#1786):** Full Starlight reference page covering message modes, decision tables, cross-project messaging, piercing rules, quarantine, hub-mode grant guard, 7 denial codes, API endpoints, permissions, and CLI commands.
* **Agent Registry lifecycle hooks example (#1791):** Auto-register Scion agents as A2A endpoints in Google Cloud Agent Registry using lifecycle hooks (register on running, deregister on stopped). Adds `PROJECT_SLUG` to trusted variables reference.
