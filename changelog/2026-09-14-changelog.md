# Release Notes (2026-09-14)

Per-user session revocation, break-glass admin promote, preview/stable release channels, and a large code-quality sweep fixing authorization, filtering, and safety defects across ~50 unified code paths.

## 🚀 Features
* **Per-user session revocation (#1590):** Adds `session_generation` counter to the User model with an admin API endpoint and UI action to force re-authentication. Includes per-request middleware generation check so revoked sessions are rejected immediately.
* **Break-glass admin promote (#1589):** Adds a break-glass `admin promote` CLI command for emergency access recovery. Prevents demotion of UI-promoted admins on config reload — `AdminEmails` is now authoritative and all-or-nothing.

## 🐛 Fixes
* **Code-quality sweep with embedded fixes (#1592):** ~50-commit cleanup removing obsolete routes, helpers, policy scaffolding, and duplicated flows across CLI, Hub, runtime, messaging, telemetry, and storage layers. Includes five behavior-affecting fixes: cross-project runtime fallback prevention, resource import authorization by kind, short chat link code masking safety, provider-specific link decoding preservation, and env key filter applied to secret metadata.
* **SSE attachment refs in chat (#1600):** `UserMessageEvent` was missing the `Attachments` field, so web chat did not receive attachment references in real time. Threaded attachment refs through `PublishUserMessage`.
* **RS1 StaleAuthority test deadlock (#1597):** Missing `CredentialContext` caused the R2-2 credential-kind gate to reject calls before `WithTx`, leaving the `txBlockStore` channel unsignaled — a 22-minute hang that timed out the entire `pkg/hub` test suite. Fixed in 4 test variants.

## 🔧 CI & Infrastructure
* **Preview/stable release channels (#1596):** Updated `build-release.yml` with two-channel release model — SemVer tag triggers, automatic release-channel detection, dynamic prerelease flag, and automatic channel branch updates (`preview`/`stable`) on each release.

## 📖 Docs
* **Nightly doc updates (#1588, #1593):** Cloud Run IAP proxy reference, legacy plaintext migration note correction, hub-wide `AllowProgeny` default, profiles env field schema rejection, and metrics dashboard admin permission documentation.
* **Messaging skill update (#1591):** Restructured inbound message types for conversation model, expanded mention handling, reframed `input-needed` as decision tree, added conversation routing examples.
