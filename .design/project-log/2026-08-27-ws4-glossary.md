# WS-4: Add conversation model terms to glossary

**Date:** 2026-08-27
**Branch:** `scion/ca-msg-em5`
**Scope:** S5 Docs — WS-4

## What changed

Added six conversation-model terms to the Messaging section of the docs-site glossary (`docs-site/src/content/docs/glossary.md`):

1. **Backfill** — the one-time process that stamps historical messages with `conversation_id`.
2. **Conversation** — the core record grouping related messages between participants.
3. **Conversation Read Switch** — the runtime flag gating read-path routing.
4. **Conversation Reference** — the recipient identifier addressing via the conversation model.
5. **Divergence** — the metric tracking agreement between legacy and conversation routing.
6. **Dual-Write** — the transitional pattern populating both routing models simultaneously.

All entries were placed in alphabetical order within the existing Messaging section, alongside the pre-existing Native Web Chat, Message Group, and Notification entries (which remain unchanged).

## Observations

- The existing `### Notification` entry is accurate and required no changes.
- The root `GLOSSARY.md` referenced in the page header is maintained separately. The new terms should be added there as part of a separate task to keep the two glossaries in sync.
