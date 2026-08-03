# Release Notes (2026-07-29)

The hub-side `gh://` skill cache completed its three-phase rollout: fallback routing infrastructure landed, Phase 2 defects were fixed (hash format, expiring URLs, cross-project authz), and Phase 3 activation flipped resolution to hub-side with context-cancellation-safe fallback. Settings precedence received a major overhaul, and the Claude harness gained proper model alias resolution.

## 🚀 Features
* **[Agent]:** Hub-side `gh://` skill cache Phase 3 activation — `RegisterFallback` routing infrastructure with context-cancellation-safe fallback, flips resolution to hub-side cache with broker-side fallback when Hub is unreachable (#900, #902).
* **[Agent]:** Deduplicate `gh://` ref→SHA lookups within a single resolve request, with case-insensitive owner/repo memo keys (#910, #912).

## 🐛 Fixes
* **[Agent]:** Fix Phase 2 hub-side `gh://` cache defects — correct hash format, handle expiring GitHub URLs, fix ref defaulting, and add cross-project authorization checks (#901).
* **[Hub]:** Settings precedence overhaul — hub agent defaults, env scope ordering fix, and thinking-level propagation correction with comprehensive design documentation (#913).
* **[Hub]:** Project settings precedence — project `default-harness-config` now correctly outranks template harness config, matching the existing precedence for max_turns, max_model_calls, max_duration, and resources (#907).
* **[Claude Harness]:** Resolve model alias and set `ANTHROPIC_MODEL` in `provision.py` — adds alias resolution using harness config model_aliases with `SCION_MODEL` env var fallback, with 280-line test suite (#908).

## 📖 Docs
* **[Skills]:** Agent lifecycle corrections — fix false safety guarantee ("committed" is not "pushed"), clarify `--preserve-branch` does not push, replace "broker slots" with "system resources" (#896, #905, #906).
* **[Docs]:** Common patterns for project shared directories — build caches, producer/consumer artifacts, shared knowledge base, file-based coordination (#903, #904).
* **[Observability]:** Clarify `gh://` cache layer routing, add broker content-cache hit/miss logging (#909).

## 🔧 Chores
* **[Deps]:** Bump postcss from 8.5.10 to 8.5.24 in web (#899).
* **[Harness]:** Update `ANTHROPIC_DEFAULT_HAIKU_MODEL` value (7eff9bb).
