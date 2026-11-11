# Release Notes (2026-09-23)

Generic JWT proxy authentication landed in two phases, hybrid-tier NFS shared directory storage shipped as Phase 1, and project file handlers were hardened against symlink traversal. Touch/mobile chat received multiple interaction fixes and a notification chime was added.

## 🚀 Features
* **Generic JWT proxy auth provider** (#1858, #1863): New `auth.proxy.provider: jwt` for operators running bespoke auth proxies. Phase 1 supports static PEM public key files with configurable header, algorithm, issuer/audience validation, and OIDC claim mapping. Phase 2 adds JWKS URL (proactive refresh, debounce, last-good fallback) and JWKS file key sources.
* **Hybrid tier phase 1: NFS shared directory storage** (#1849): Global `server.shared_dir_storage` setting for NFS-backed shared directories. Docker agents bind-mount and K8s pods use a static PVC with subPath, resolving to the same project path on a shared NFS export. Symlink-safe openat path confinement, fails closed.
* **Browser notification chime** (#1861): Web Audio API synthesized two-tone chime (E5 to G5, 150 ms) on SSE message arrival from other users. Global and per-project toggles, 2-second cooldown, graceful autoplay policy handling.
* **Thread default agent toolbar** (#1865): Shows terminal and graph buttons in thread toolbar when a default agent is configured, matching existing DM behavior.
* **Project agents expand/collapse** (#1860): Adds expand/collapse toggle for the project agents section with localStorage-persisted preference across projects.

## 🔒 Security
* **Symlink traversal hardening** (#1850): Confines hub project file handlers to served directories using `os.Root` per-component kernel-enforced containment. Covers list, download, archive, upload, write, and delete across workspace and shared directory bases. Symlinks resolving outside the served directory are refused and logged.

## 🐛 Fixes
* **Agent message delivery honesty** (#1868): Rejects agent-to-agent messages to non-running agents with a DELIVERY_FAILED notice instead of silently dropping. Brokers report buffered flush failures back to the hub, which marks rows failed and notifies senders.
* **Dispatch skill resolution** (#1845): Resolves Hub-registry skill refs at dispatch as the agent's creator instead of the broker's identity, fixing authz denials on required non-public skills. Pre-resolved results are authoritative with fail-closed semantics.
* **Group conversation convergence** (#1864): Converges default agent across both stores in one transaction, adds project-based group reads for any project member, and tracks agents dispatched into groups as participants.
* **Cloud Run proxy GCS dependency** (#1867): Builds the IAP proxy image on the VM and deploys with `--image` instead of `--source`, eliminating the GCS dependency that enterprise org policies block.
* **Deploy config simplification** (#1862): Switches deploy config from YAML to JSON (drops PyYAML/PEP 668 dependency), defaults release_channel to nightly for git-clone deployments.
* **Touch/mobile chat interactions** (#1857, #1859): Enter key now inserts newlines on touch devices instead of sending (Send button remains); message context menu accessible via tap on touch-primary devices with shadow DOM retargeting.
* **Member sidebar flicker** (#1856): Reconciles stale stateManager agent entries against authoritative server responses, preventing infinite add/remove sidebar flicker when SSE deleted events are missed.
* **Promote-DM-to-thread** (#1855): Fixes Postgres 42883 cast error (text vs uuid) in message re-key UPDATE and closes the promote dialog before the SSE navigation race can leave it open.
* **Compat-literals CI fix** (#1854): Replaces legacy grove literals introduced by the NFS shared_dir_storage feature with project literals to fix check-project-compat-literals.
* **Emulated build leg Chromium** (#1848): Skips Chromium headless smoke test on emulated (QEMU) build legs where ptrace is unavailable; retains ELF arch, library, and version assertions.

## 📖 Docs
* **Runbook improvements** (#1866, #1851): Adds cross-org IAP preflight check, corrects build time estimates (10-15 min), adds agent URL-fetching warning, batch-question guidance, and repo clone step for URL-based runbook access.
* **Nightly doc update** (#1853): Nightly update for 2026-09-22 covering the agent.lifecycle/attach permission split, binary auto-update, headless VM deploy, timezone injection, responsive header, and terminal layout URLs.
