# Release Notes (2026-08-21)

A critical security sweep closed 22 authorization bypass sites across the hub API, the authz-guard regression check was wired into CI, and hub agent defaults gained model and thinking-level stamping.

## 🔒 Security
* **[Hub]:** Fix 22 authorization guard bypasses and wire CI regression check — every site used `GetUserIdentityFromContext` which returns nil for agent and broker callers, silently skipping authorization. All 22 handlers across 9 files converted to fail-closed `s.authorize()` helpers. `checkBrokerDispatchAccess` fail-open eliminated, `addGroupMember` caps non-user callers at plain member role, and 404-before-403 isolation ordering enforced. 462 lines of regression tests (7 suites, 35 cases) plus CI wiring via Makefile target and GitHub Actions step (#1244).

## 🐛 Fixes
* **[Hub]:** Apply hub default model and thinking level to agents — `applyHubAgentDefaults` now stamps `DefaultModel` and `DefaultThinkingLevel` onto agents (only-if-empty guard). `extractAgentDefaults` includes `default_model`, `default_thinking_level`, `default_max_agent_role`, and `default_agent_role` in its field list, fixing silent drops during file-to-DB seeding (#1245).
* **[Web]:** Show `register_url` field on Discord and Telegram admin settings forms — field was only rendered when a value already existed, making first-time entry through the UI impossible (#1243).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 20 — secret scope precedence, git credential NoAuth exemption, user-scoped template ordering, skill AllowProgeny, project-scoped interagent queries, Gemini 3.7 Flash model bump (#1242).
