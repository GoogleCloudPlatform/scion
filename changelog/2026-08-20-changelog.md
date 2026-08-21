# Release Notes (2026-08-20)

A critical secret scope precedence fix corrected three independent inversion bugs, user-scoped template ordering and progeny flags shipped, and git credentials were exempted from NoAuth secret suppression to fix clone-per-agent workspace failures.

## 🚀 Features
* **[Hub]:** User-scoped template ordering, skill and env-var progeny flags — user templates cluster at top of new-agent dropdown (user > project > global with visual separators), user-scoped injected skills gain `AllowProgeny` flag for propagation to child agents, and env vars gain the same progeny flag when `injectionMode=always`. All three follow the existing secrets progeny-policy pattern (#1228).
* **[Harness]:** Bump antigravity medium model from Gemini 3.6 Flash to Gemini 3.7 Flash — updates both `config.yaml` alias and `provision.py` default (#1240).

## 🐛 Fixes
* **[Secret]:** Correct inverted scope precedence in `Resolve()` — scope precedence was inverted in three independent places (`LocalBackend.Resolve`, `GCPBackend.Resolve`, and dedup logic), all putting `runtime_broker` (weakest scope) ahead of `project` and `user`. Fixed to match the documented order: `runtime_broker < hub < project < user` (#1227). Follow-up updates five stale scope-precedence comments across `pkg/hub` and `pkg/secret` (#1236).
* **[Hub]:** Exempt git credentials from NoAuth secret suppression — when `NoAuth=true` (auto-fallback for missing LLM credentials), `buildCreateRequest` blanket-suppressed all secrets including `GITHUB_TOKEN`, causing git clone failures in clone-per-agent workspaces. Now resolves `GITHUB_TOKEN` from project secrets with user-scope fallback. Secondary fix: `isAuthTypeSatisfied` skips env-var checks for GCP-backed auth types when verified GCP SA is assigned, preventing false NoAuth triggers (#1237).
* **[Chat]:** Scope interagent message queries by project — slug-based backward-compat query (`Sender: agent:slug`) returned messages from all projects with identically-named agents. Both query paths now include `ProjectID` filter (#1220).
* **[Web]:** Use standard radio buttons instead of radio-button pills for the capture auth scope dialog per reviewer feedback (#1238).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 19 — secret scope selection in capture auth, project context preservation in chat navigation (#1234).
