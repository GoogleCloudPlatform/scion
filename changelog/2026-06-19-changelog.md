# Release Notes (2026-06-19)

Harness config management gained delete and image status UI, agent logs got a broker-based fallback, and skill version publishing became idempotent for interrupted drafts.

## 🚀 Features
* **[Web]:** Harness-config detail page improvements — delete button with confirmation dialog, image status section showing image path (local vs remote) and last update time, and `HarnessConfigData` type to surface `config.image` from the API. Agent detail logs tab now always visible, falling back to broker-based `/api/v1/agents/{id}/logs` when Cloud Logging is not configured (#452).

## 🐛 Fixes
* **[Hub]:** Made skill version publish idempotent for draft versions — retrying after an interrupted upload/finalize now returns the existing draft with fresh upload URLs instead of a 409 Conflict. Published/deprecated/archived versions still reject duplicates (#451).
* **[Hub]:** Added missing user-scope authorization check in `deleteHarnessConfig` and defaulted `deleteFiles` checkbox to unchecked so users must opt in to file deletion (#452).
