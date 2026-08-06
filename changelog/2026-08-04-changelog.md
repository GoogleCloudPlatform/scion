# Release Notes (2026-08-04)

A landmark observability day: the integrated diagnostics dashboard and health monitoring system both shipped, server-side OTel tracing landed for Hub and Broker, and a comprehensive sweep fixed Cloud Logging duplicate entries, label deduplication, log scoping, and trace correlation. The web terminal gained drag-and-drop file upload, and enterprise OAuth cookie-overflow login loops were resolved.

## 🚀 Features
* **[Hub]:** Integrated diagnostics dashboard with unified log aggregation — batch and SSE log API endpoints, source classification (hub/broker/agent), real-time streaming, level filters, search, and Cloud Logging popout link. 2073 lines across 9 files (#1022).
* **[Hub]:** Health monitoring dashboard, `scion doctor` checks, and Cloud Monitoring alert policies — 8 CLI-side and 3 in-container doctor checks, 15 alert policy definitions, 4 uptime check configurations, admin health summary API, and web UI health page. 2593 lines across 12 files (#1021).
* **[Telemetry]:** Server-side OTel tracing spans on Hub and Broker request handlers — TracerProvider setup, HTTP tracing middleware, operation-level spans on agent CRUD/project ops/session management, cross-component trace propagation (#1016). OTel export support added to Copilot CLI harness following the established Codex/Claude pattern (#1017).
* **[Web]:** Drag-and-drop file upload to web terminal — uploads to shared directory via existing API, injects container path into terminal input. Window-level drag prevention, shell quoting, 50MB/file and 100MB total limits, scratchpad preference, graceful degradation for non-co-located brokers. 22 new tests (#1019).
* **[CLI]:** `scion hub shadow`/`unshadow` commands for local directory links to remote hub projects — enables `scion list`, `scion look`, and `scion attach` on workstations without a local broker (#1020).
* **[Hub]:** Configurable auto-suspend stalled threshold — new `stalled_threshold` lifecycle setting with env var, JSON schema, admin UI input, and 2-minute minimum validation (#1031).
* **[Hermes]:** Web dashboard sidecar — starts Hermes dashboard on port 9119 as a scion service, auto-exposed via hub reverse proxy (#1036).
* **[Antigravity]:** ADC (Application Default Credentials) auth support — independent auth path from keyring/OAuth using `AGY_ADC_AUTH` and standard Google Cloud credentials. AGY CLI bumped from 1.1.0 to 1.1.10 (#1047).

## 🐛 Fixes
* **[Logging]:** Comprehensive Cloud Logging fix sweep — suppress duplicate entries on Cloud Run by skipping stdout handler when direct API handler exists (#1029); add `hub` label to agent log entries so they appear in diagnostics UI (#1039); scope diagnostics log filter to Scion log names only and add cross-hub isolation via `labels.hub` filter (#1032); remove legacy `grove` labels and deduplicate project/agent ID labels (#1049); add `gcp.project_id` resource attribute for clickable Cloud Trace links (#1042); demote broker `auth_success` from Info to Debug to reduce noise (#1043).
* **[Hub]:** Enterprise OAuth cookie-overflow fix chain — restore retry-without-tokens fallback in `handleOAuthCallback` for enterprise Google accounts exceeding securecookie's 4096-byte limit (#1041); generate per-request Bearer tokens for overflow sessions to prevent 401 login loops (#1048).
* **[Hub]:** Resolve `as_needed` env vars stored under `any_of` alternative key names — `resolveAsNeededForKeys` now expands lookup with alternatives and maps back to canonical keys (#1028).
* **[Hub]:** Clean up host directory when shared dir is deleted via API — previously only removed the DB record, leaving orphan directories (#1046).
* **[Logging]:** Configurable slow request threshold (default 10s, was hardcoded 2s) and exempt streaming responses (`text/event-stream`) from slow request logging (#1030).
* **[Sciontool]:** Resolve credential timing race — reorder init startup so staged-secrets writing happens before telemetry pipeline initialization (#1045).
* **[Broker]:** Use detected container runtime for heartbeat instead of hardcoded default — fixes errors on podman-only and non-standard-path hosts (#1018).
* **[Agent]:** Guard terminal-state persist behind file-existence check to eliminate guaranteed-to-fail `os.ReadFile` calls (#1044).
* **[Discord]:** Add `CREATE_PUBLIC_THREADS` permission pre-check for `/scion thread` with improved 403 error messages and updated invite UI permissions list (#1037).
* **[Web]:** Register missing `heart-pulse` and `journal-text` Shoelace icons for production builds (#1033). Add fixed height and scroll to project page agent list (#1035).

## 📖 Docs
* **[Docs]:** Publish weekly release notes format and archive daily entries — transitions docs site from daily to weekly summaries (#1025). Nightly documentation update for Aug 2-3 changelogs (#1024).
* **[Docs]:** Add `PAGE_TITLES` documentation (`web/AGENTS.md`) and root `AGENTS.md` index to prevent "Page Not Found" pitfall (#1040).

## 🔧 Chores
* **[Deps]:** Bump postcss to 8.5.25 in docs-site (#1027). Bump fast-uri to 3.1.5 in docs-site (#1026).
