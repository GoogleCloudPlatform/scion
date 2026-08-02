# Release Notes (2026-08-01)

A fix-heavy day resolving a chain of auth corruption bugs affecting Vertex AI agents, enforcing the `as_needed` injection mode for secrets, and hardening the Discord integration. Two new features landed: agent port forwarding through the Hub and configurable scheduler concurrency to prevent DB saturation on small deployments.

## 🚀 Features
* **[Hub]:** Agent port forwarding — agents can now expose local HTTP ports through the Hub as authenticated, reverse-proxied URLs (#937).
* **[Scheduler]:** Configurable scheduler interval and per-task concurrency (`max_concurrency`, default 2). Adds jitter before semaphore acquire to avoid thundering-herd. Fixes DB connection pool saturation on small deployments (#935).

## 🐛 Fixes
* **[Auth]:** Guard harness auth corruption and add Vertex AI credential translation — fixes self-perpetuating corruption where harness implementation names (e.g. `container-script`) leaked into `opts.HarnessAuth` and `scion-agent.json`, causing Vertex AI agents to fail with "not logged in" on every restart. Adds `GOOGLE_CLOUD_PROJECT` → `ANTHROPIC_VERTEX_PROJECT_ID` translation for Claude harness (#939). Follow-up fix prevents auth backfill from writing the implementation name as auth type, with a defensive guard rejecting known harness names (#936).
* **[Hub]:** Enforce `as_needed` injection mode for env vars and secrets — previously the annotation was stored and shown in the UI but not filtered at dispatch time. Adds filtering at all three injection paths in `httpdispatcher.go` (#944).
* **[Hub]:** Validate `image_registry` before starting runtime broker — fails fast with an actionable error instead of silently starting and causing opaque image-pull 404s later (#949).
* **[Discord]:** Prevent `threadParents` negative-cache poisoning — transient Discord API errors no longer permanently block thread parent lookups. `threadParentID()` now returns `(string, bool)` to distinguish confirmed results from failures (#947).
* **[Discord]:** Inherit IAP transport in `longHTTPClient` — fixes 401 errors on IAP-protected deployments (Cloud Run with IAP) for long-running operations like `CreateAgent` (#946).
* **[Discord]:** Use configurable `register_url` for user-facing registration links instead of internal `hub_url`, fixing broken links behind auth proxies (#948).
* **[Discord]:** Make `/scion send` search root configurable via `send_search_root` config field, defaulting to `/scion-volumes/` (#945).
* **[Messages]:** Preserve mention metadata in delivery messages — CC'd agents now correctly see the primary recipients (#943).
* **[Ops]:** Add certbot `--deploy-hook` for automated Caddy cert reload after renewal, preventing HTTPS failures from stale certificates on GCE hub instances (#942).
* **[CLI]:** Validate `--attach` file paths (existence, allowed roots, regular file) before sending message — previously exited 0 with "Message sent" for nonexistent files (#941).
* **[Config]:** Warn when settings file has real configuration keys but no `schema_version` — advisory only, no behavior change (#940).
* **[Init]:** Stop blaming `GITHUB_TOKEN` for unclassified git clone errors — fallback now shows a neutral "unclassified error" label (#938).
