# Release Notes (2026-06-23)

A light day with fixes to metrics reporting and capture-auth error handling.

## 🐛 Fixes
* **[Metrics]:** Fixed 7 review findings — handle all `KeyValue` types in `attrSetKey` (bool, double, zero int, empty string), simplify duration alignment to use `Truncate`, remove raw error messages from `X-Metrics-Warning` HTTP headers, and move chart rendering side-effects from `render()` to `updated()` lifecycle.
* **[Capture Auth]:** Treat "already exists" as successful capture — previously returned `(False, None)` causing the main loop to trigger a keyring fallback that also failed on the same condition (#479).
