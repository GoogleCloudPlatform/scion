# Release Notes (Mar 17, 2026)

This release introduces a major new GCP Identity implementation allowing agents to authenticate via metadata server emulation, alongside comprehensive new Grove Settings and Agent Limits configurations in the UI.

## 🚀 Features
* **GCP Identity & Metadata Emulation:** Implemented end-to-end GCP identity assignment for agents using metadata server emulation and token brokering. This includes a new Web UI for Service Account management, iptables interception, per-agent rate limiting, audit logging, and telemetry metrics (consolidated from commits 2ac33bb, 961653a, d37a79c, d11318f, a5f457a, d187838, 8df2a04, 34c7056, 401a178, 52f6838).
* **Grove Settings & Agent Limits:** Introduced a comprehensive Grove Settings UI organized into General, Limits, and Resources tabs. Administrators can now configure default agent limits at both the hub and grove levels, which automatically pre-populate when creating new agents (consolidated from commits c7d9585, aa5c2ff, 2ffdff8, 8f0263f, 0d87a17, 07714a1, 906a88d).
* **Workspace Content Previews:** Added content preview capabilities for workspace files directly within the UI (commit 53cea7c).
* **CLI Enhancements:** Added a `-r`/`--running` flag to the `scion list` command to easily filter for active agents (commit 7001035).

## 🐛 Fixes
* **Grove & Membership Synchronization:** Resolved multiple issues with grove linking and membership backfills, including fixing unique constraints on grove IDs, ensuring proper legacy owner role assignments, and correctly including auto-provide brokers (consolidated from commits 4af2662, 307fb85, cb22a18, 79cc591, 1f6f16f, e14ec95).
* **Storage & ID Consistency:** Fixed global grove ID bleed-through issues and unified agent split storage paths under `.scion/` for deterministic behavior across hub-managed and external groves. Ensured cascading cleanups of templates and configs when a grove is deleted (consolidated from commits fea4588, 6bb2348, a97ebd7, 023a089, 6eaf8dc, 221c736, 75bfcc0, c9d8ddf).
* **GCP Validation & Logging:** Improved debug logging for 4xx errors and enhanced GCP Service Account validation messages, including returning capabilities in the list API response (consolidated from commits e060664, d65dc09).
* **Container Lifecycle Management:** Ensured agent containers are gracefully stopped before removal to prevent shared-directory mount errors (commit 8a0fabc).
* **Template Synchronization:** Fixed an issue where template synchronization was blocked by setting a default image for the generic harness config (commit 816c960).
* **Web UI Consistency:** Fixed layout issues such as status column widths in agent tables and exposed Scion version information on the admin config page (consolidated from commits 53f55b5, 7536c59).
