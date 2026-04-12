# Release Notes (2026-04-03)

This release focuses on hardening GCP identity management, introducing Hub-minted service accounts, and significantly expanding the web-based file and template editing capabilities.

## ⚠️ BREAKING CHANGES
* **Authentication:** The Hub secret used for Application Default Credentials (ADC) has been renamed from `GOOGLE_APPLICATION_CREDENTIALS` to `gcloud-adc`. This secret now writes directly to the well-known GCP config path (`~/.config/gcloud/application_default_credentials.json`), and the `GOOGLE_APPLICATION_CREDENTIALS` environment variable is no longer set by default.
* **Security:** Agent API access to Hub secrets is now explicitly blocked to improve isolation and prevent credential leakage.

## 🚀 Features
* **GCP Identity & Hub-Minted Service Accounts:** 
    * Implemented Hub-minted GCP service accounts (Phases 1 & 2), allowing the Hub to manage and provision service accounts directly.
    * Added a new admin quota dashboard and minting capability controls.
    * Enabled the use of GCP service accounts as a direct authentication source for Vertex AI.
* **Web File & Template Editor:** 
    * Launched a comprehensive inline file editor (Phase 2) with integrated Markdown preview (Phase 3).
    * Added fuzzy and regex-based file name filtering to the file browser.
    * Introduced template file browsing, editing, and upload capabilities.
    * Extracted the file browser into a shared component for consistent usage across the UI.
* **UI & Configuration Visibility:** 
    * Agent detail cards now explicitly show the active authentication method.
    * The GCP identity option is now always visible in agent configuration forms to simplify onboarding.
* **Agent Poker Enhancements:** Improved simulation flow with automated communication reminders and better handling of stalled players.

## 🐛 Fixes
* **GCP Metadata Server Emulation:** 
    * Fixed `numeric-project-id` reporting to ensure `gcloud` correctly identifies the GCE environment.
    * Scoped metadata blocking rules to TCP/80 to avoid interfering with container DNS resolution.
    * Improved discovery by setting `GCE_METADATA_ROOT` and ensuring recursive service-account paths return valid JSON.
    * Corrected ADC file handling when clearing `gcloud` configuration.
* **Security Hardening:** Prevented Hub signing keys from being exposed to agent environments.
* **Web UI Refinements:**
    * Renamed "Add Service Account" to "Register Existing" to better reflect the action.
    * Fixed dark mode syntax highlighting in the code editor.
    * Added missing GCP identity fields to configuration interfaces.
* **System Stability:** 
    * Enforced cascade deletion of agents when a Grove is deleted.
    * Fixed several edge cases in the "Agent Poker" simulation, including auditor hole-card leaks and false-positive bans.
