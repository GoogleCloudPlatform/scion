# Release Notes (2026-06-17)

A targeted fix for message display in the agent detail view, improving both data completeness and access control.

## 🐛 Fixes
* **[Messaging]:** Fixed message display in agent detail view — web UI messages now default to `channel: "web"` and persist `channel`/`threadID` fields on message records. Agent managers (owners, project admins, global admins) can now see all messages including those from chat integrations, while other users only see messages where they are a participant (#435).
