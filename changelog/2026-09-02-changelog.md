# Release Notes (2026-09-02)

Light day — one user-facing fix and a documentation update.

## 🐛 Fixes
* **Agent-create template list empty (#1454):** The agent-create form requested templates with `limit=200`, but the backend authorized list validator caps at 100 and returns HTTP 400. The raw `fetch()` silently swallowed the error, leaving the template dropdown empty.

## 📖 Docs
* **Message formatting guidance (#1451):** Added a Message Formatting section to the scion-messaging skill documenting that the CLI delivers message bodies verbatim (no escape expansion), with correct/wrong pattern examples.
