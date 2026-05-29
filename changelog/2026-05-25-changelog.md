# Release Notes (2026-05-25)

This period focused on performance optimizations and reliability improvements for the CLI and agent dispatch pipeline, specifically targeting startup delays and broker stability in large workspaces.

## 🚀 Features
* **Performance Monitoring:** Added timing instrumentation to the agent dispatch pipeline and CLI-side workspace scan, enabling detailed performance analysis of agent lifecycles and file operations.

## 🐛 Fixes
* **CLI Startup Optimization:** Eliminated significant startup delays when running commands within hub-connected containers by skipping redundant local file scanning and hashing.
* **Broker Stability & Reliability:** Resolved "no_runtime_broker" errors and broker lockups during concurrent agent operations. Agent-specific commands now bypass unnecessary full-workspace synchronizations, preventing the broker from being overwhelmed by synchronous filesystem walks.
