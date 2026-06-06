# Release Notes (2026-06-01)

This release focuses on significant architectural cleanup, specifically disambiguating the "broker" terminology, alongside major enhancements to resource management and new support for Google Cloud Storage.

## ⚠️ BREAKING CHANGES
* **CLI / Internal API:** The `scion broker` command has been renamed to `scion runtime-broker` to avoid confusion with Message Brokers. While a deprecated alias remains for now, users should update their scripts. Internally, `pkg/broker` has been renamed to `pkg/eventbus`, and related types (e.g., `MessageBroker` -> `EventBus`) have been updated.

## 🚀 Features
* **Resource Management Overhaul:** Major refactor of resource storage, caching, and import logic. This includes support for hub-level imports, improved progress tracking, and significant performance optimizations for large resource sets.
* **Google Cloud Storage (GCS) Support:** Introduced native support for GCS resources, allowing agents to interact directly with GCS buckets.
* **Hub UI Improvements:** Added a new collapsible side panel to the Hub web interface for better screen real estate management.
* **Messaging & Broker Plugins:** Implemented chat channel routing for broker plugins, enabling more sophisticated message orchestration.
* **Harness Observability:** Added content-type filtering for assistant responses, improving the granularity of observability logs.
* **Engineering Glossary:** Introduced `GLOSSARY.md` to establish canonical terminology across the codebase and documentation.

## 🐛 Fixes
* **Agent Lifecycle:** Resolved a critical bug where the `resume` command would incorrectly create new agents instead of restarting existing stopped ones.
* **Stability:** Fixed a Hub crash that occurred when the Cloud Logging service experienced metadata outages.
* **Auth & Integration:** 
    * Added `issues:write` to default GitHub App token permissions to support issue-tracking features.
    * Fixed remote template imports by adding `GITHUB_TOKEN` secret fallback support.
