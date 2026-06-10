# Release Notes (2026-06-08)

A major day for infrastructure and reliability: Kubernetes worktree-per-agent support landed, the hub gained configurable lifecycle hooks, and a concentrated burst of fixes resolved GCP auth failures on agent resume — addressing metadata server races, stale port reclamation, and OIDC routing conflicts.

## 🚀 Features
* **[Runtime]:** Worktree-per-agent isolation on Kubernetes — each agent gets its own git worktree via NFS-backed provisioning, preventing workspace conflicts between concurrent agents (#356).
* **[Hub]:** Configurable agent lifecycle hooks — project admins can now define webhook-style hooks that fire on agent phase transitions (start, stop, suspend, error), with a full validation framework and variable interpolation engine (#357).
* **[Hub]:** Auto-suspend controls for stalled agents — new admin toggle (default: off) to automatically suspend agents detected as stalled, with harness resume-capability checks to prevent suspending agents that can't be meaningfully resumed (#359, #361).
* **[Docs]:** Comprehensive agent lifecycle documentation covering suspend/resume, crash recovery, error phase semantics, and auto-suspend behavior (#358).

## 🐛 Fixes
* **[Auth]:** Restored GCP auth on agent resume by always starting the token refresh loop even when the persisted token has expired, and enhanced `sciontool doctor` to verify end-to-end GCP token acquisition (#360).
* **[Auth]:** Routed metadata server GCP token requests through the hub client instead of direct HTTP, fixing OIDC transport auth conflicts on Cloud Run/IAP deployments (#364).
* **[Auth]:** Fixed hub client initialization race — `hubClient` is now created before the metadata server starts, eliminating a data race on concurrent HTTP handler goroutines (#366).
* **[Auth]:** Skip OIDC metadata mode when the scion metadata server is active, preventing timeout loops caused by the iptables redirect making the real GCE metadata endpoint unreachable (#367).
* **[Runtime]:** Metadata server port reclamation on resume — added `/_scion/shutdown` endpoint and retry-with-backoff logic so a fresh init cycle can reclaim port 18380 from a stale instance (#368).
* **[Runtime]:** Made metadata server `Stop()` synchronous and added same-process reclaim via Go reference, fixing cases where the HTTP shutdown endpoint returns 404 on older binaries (#369).
* **[Runtime]:** Treat signal-killed child process as clean exit during intentional shutdown, preventing agents from cycling through PhaseError before reaching their intended stopped/suspended state (#370).
* **[Runtime]:** Removed unconditional auto-suspend handler that bypassed the admin toggle, consolidating auto-suspend into the single toggle-gated path (#365).
* **[Networking]:** Routed colocated Docker agents to bridge networking via Caddy domain, fixing port conflicts and GCP identity leaks when multiple agents ran with `--network=host` (#371).

## 🔧 Chores
* **[CI]:** Applied `gofmt` to 7 files failing format checks on main (#373).
* **[Harness]:** Updated Codex harness default model to gpt-5.5 (#374).
* **[Docs]:** Backfilled changelog entries for June 6-7 (#372).
