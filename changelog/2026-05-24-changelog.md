# Release Notes (2026-05-24)

This update focuses on identifying and eliminating latency in the agent startup process, significantly improving performance in hub-connected environments.

## 🚀 Features
* **Timing Instrumentation:** Added comprehensive latency tracking across the agent dispatch pipeline, including worktree creation, provisioning, and broker round-trips, to help diagnose startup bottlenecks.
* **CLI Performance Metrics:** Integrated debug timing logs for CLI-side workspace scanning and suspended-agent checks.

## 🐛 Fixes
* **Eliminate Startup Delays:** Optimized agent startup by skipping redundant workspace file scans when running within a hub context. This eliminates delays (sometimes exceeding 16 seconds) caused by hashing large project files that are already accessible to the broker.
