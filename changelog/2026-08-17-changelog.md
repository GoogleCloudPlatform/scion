# Release Notes (2026-08-17)

A major day: the Scion Helm chart for GKE shipped (Phase 0), the native web chat feature gap programme advanced through Phases 0–5 with 10 PRs, GKE/HA deployment gained critical readiness and preflight fixes, and a shellcheck CI gate cleared 85 findings across all 49 repository shell scripts.

## 🚀 Features
* **[Helm]:** Helm chart for deploying the Scion hub to GKE (Phase 0) — chart templates, `values.schema.json` for install-time validation, 16 operator-facing validation claims, verification suite, readiness probe targeting `/readyz`, stable `hub_id` across upgrades. Unsupported shapes refused at render time rather than failing silently at runtime (#1195). Disambiguate truncated cluster-scoped RBAC names — `ClusterRole`/`ClusterRoleBinding` names that truncate to 63 bytes now append a digest over fullname/namespace to prevent cross-namespace collisions that could repoint `pods/exec` and `secrets` authority (#1212).
* **[Web Chat]:** Feature gap programme Phases 0–5 — Phase 0 fixes scrollback pagination (client/server cursor mismatch), scroll-to-bottom, wires real PresenceChecker, and adds per-sender token-bucket rate limiting (#1200). Phase 1 adds conversation mute, thread pinning, custom space ordering with drag-and-drop, and expanded attachment allow-list (34 developer file types) with per-file partial success (#1201). Phase 3 adds per-message action bar (hover + long-press), reply/quote with backend support, edit/delete for own messages, unread divider with watermark, and message permalinks (#1209). Phase 4 adds paste-to-upload, 16K character limit, SSE direct append, idempotency keys, syntax highlighting, and Cmd/Ctrl-K conversation switcher (#1211). Phase 5 adds deep links from agent messages, rich agent output rendering (diffs, test results, JSON/YAML), thread export as markdown, send-to-agent context, and slash commands (#1210).
* **[Web Chat]:** Browser notifications for mentions and DMs with tab title unread badge, permission management, mute-aware and active-conversation suppression, and desktop notification toggle (#1207). Markdown attachment rendering with source/preview toggle and clipboard copy (#1208).
* **[Server]:** Warn at startup when IAP audience looks like a synthetic bootstrap placeholder, supporting the GKE two-step bootstrap flow (#1191).
* **[CLI]:** Accept repeated values for five string-slice flags (`--admin-emails`, `--cc`, `--scopes`, `--triggers`) — switches from `StringVar` to `StringArrayVar` with `splitCommaList` helper preserving backward compatibility. Stricter empty-value validation (#1206).

## 🐛 Fixes
* **[Hub]:** Handle `gke-shared-volume` workspace backend in readiness check and hub project paths — two switch statements never learned about this backend, causing permanent 503 on `/readyz` and silent data loss to ephemeral storage on reschedule (#1194).
* **[Hub]:** Do not require IAP for hosted HA preflight — split `validateHostedHAPreflight` into universal HA checks and IAP-specific checks, enabling OAuth/OIDC-direct HA deployments (#1193).
* **[Hub]:** Persist GitHub App config to the DB in postgres mode (#1198).
* **[Server]:** Accept truthy env values (`yes`/`y`/`on`, case-insensitive) for four boolean env vars via `parseBoolEnv` helper, with warnings on unrecognized non-empty values (#1192).
* **[Monitoring]:** Fix uptime check content matcher that could never fail on degraded status — replace `CONTAINS_STRING` with `MATCHES_JSON_PATH` + `EXACT_MATCH` on `$.status` (#1202).
* **[Web Chat]:** Fix mention leak into default agent tab and add per-thread draft persistence (#1215). Action bar positioning, dark theme artifact, routing header retroactive leak, and restore markdown attachment preview (#1216). Switch attachment uploads from MIME allow-list to deny-list (block executables only) (#1217).
* **[CLI]:** Fix `--harness` flag defaulting to loop variable `h` instead of empty string in `scion create` (#1204).
* **[Hub]:** Eliminate scheduler timer test flake — race between claim and handler resolved with test-only synchronization (#1203).
* **[Hack]:** Split `check-authz-guards` refusal exit codes (3=tool-absent, 2=corpus problem) and print on both stdout and stderr (#1197).

## 🔧 CI & Infrastructure
* **[CI]:** Add shellcheck gate and fix all 85 findings across 49 shell scripts (10,073 lines) — real defects include `SC2286` executing empty string as command, `SC2013` word-splitting on filenames with spaces, `SC2064` traps expanding at definition time. 38 documented suppressions with rationale. Positive control verifies gate can fail (#1205).
* **[CI]:** Add Helm chart CI workflow — Helm lint, template rendering across three fixtures (including 53-char release name for truncation testing), kubeconform validation, values.yaml schema validation, and yamllint (#1213, #1214).

## 🧪 Tests
* **[Hub]:** Add tripwire test for `isHADeployment` route inventory — catches new routes that bypass HA checks (#1199).

## 📖 Docs
* **[Docs]:** Weekly release notes for Aug 10–16 — native web chat launch, Teams and Google Chat integrations, multi-instance HA hardening, GCP SA IAM gates (#1196).
