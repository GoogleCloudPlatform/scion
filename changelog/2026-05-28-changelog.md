# Release Notes (2026-05-28)

This release focuses on identifying and eliminating latency within the agent startup process and the message dispatch pipeline. By introducing comprehensive timing instrumentation and optimizing CLI operations in hub-connected environments, this update significantly improves performance and stability for large-scale workspaces.

## 🚀 Features
* **[Performance]: Timing Instrumentation.** Added detailed elapsed-time logging across the entire agent dispatch pipeline.
    * **Hub & Broker:** Tracks pre-dispatch setup, broker round-trip times, and manager startup durations.
    * **Provisioning:** Measures worktree creation, home directory composition, and skill copying.
    * **CLI Monitoring:** Integrated debug timing logs for CLI-side workspace scanning and suspended-agent checks to identify bottlenecks.
* **[Messaging]: Multi-Broker Awareness.** Foundational improvements to how the Hub interacts with external brokers, preparing for fanned-out message delivery and observer-mode monitoring.

## 🐛 Fixes
* **[CLI]: Startup Optimization.** Eliminated redundant workspace file scanning when the CLI is running within a hub-connected container. By skipping expensive SHA-256 hashing of large project files already accessible to the broker, startup delays (which could exceed 16 seconds) have been removed.
* **[Infrastructure]: Broker Stability.** Resolved "no_runtime_broker" errors caused by broker saturation. Agent-specific commands (start, stop, message, etc.) now bypass unnecessary full-workspace synchronizations, preventing synchronous filesystem walks from blocking heartbeats.
* **[Harnesses]: Type Passthrough.** Fixed an issue where custom harness types were being dropped during dispatch; non-canonical harness names are now correctly propagated from the Hub database to the runtime.
* **[Configuration]: Credentials Priority.** Ensured that `SCION_HUB_TOKEN` and `SCION_HUB_ENDPOINT` environment variables correctly override stale `hub.enabled=false` settings in configuration files.
