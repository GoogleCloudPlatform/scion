# Phase 1 — Ent Schemas for Messaging Conversation Model

**Date:** 2026-08-27  
**Branch:** `dev-schema` (based on `origin/scion/messaging-v2`)  
**Commit:** `feat(messaging): add conversation model ent schemas (Phase 1)`

## Summary

Created three new ent schemas for the messaging conversation model as specified
in the design doc (§2.2, §2.3). This is purely additive — no existing schemas
or code paths were modified.

## Schemas Created

### 1. `Conversation` (`pkg/ent/schema/conversation.go`)

Table: `conversations`

Core fields: `id` (UUID), `project_id` (optional UUID), `kind` (enum:
direct/group), `surface` (enum: native/discord/slack/telegram/gchat/teams),
`external_ref`, `parent_ref`, `display_name`, `default_agent_id` (UUID, not
slug), `drift_state` (enum: active/orphaned/unresolvable), timestamps
(`last_activity_at`, `created_at`, `archived_at`, `deleted_at`).

Indexes:
- **Partial unique index** on `(surface, external_ref)` WHERE
  `external_ref <> '' AND deleted_at IS NULL` — implemented using
  `entsql.IndexWhere()`, which is natively supported in ent v0.14.5.
  Both SQLite and Postgres support this WHERE clause syntax.
- Index on `project_id`
- Index on `kind`

### 2. `ConversationParticipant` (`pkg/ent/schema/conversation_participant.go`)

Table: `conversation_participants`

Fields: `id` (UUID), `conversation_id` (UUID), `principal_kind` (enum:
user/agent), `principal_id` (string), `role` (enum: member/observer),
`joined_at`, `left_at` (optional).

Indexes:
- Unique on `(conversation_id, principal_kind, principal_id)`
- Index on `principal_id`

### 3. `MessageAddressee` (`pkg/ent/schema/message_addressee.go`)

Table: `message_addressees`

Fields: `id` (UUID), `message_id` (UUID), `principal_kind` (enum: user/agent),
`principal_id` (string), `via` (enum: explicit/body-mention/default-agent/direct),
`delivery_state` (enum: pending/delivered/failed), `failure_reason` (optional).

Indexes:
- Unique on `(message_id, principal_kind, principal_id)`
- Index on `message_id`
- Composite index on `(principal_kind, principal_id, delivery_state)`

## Partial Index Approach

The critical partial unique index on `(surface, external_ref)` is expressed
using ent's built-in `entsql.IndexWhere()` annotation:

```go
index.Fields("surface", "external_ref").
    Unique().
    Annotations(
        entsql.IndexWhere("external_ref <> '' AND deleted_at IS NULL"),
    ),
```

This is fully supported by ent v0.14.5 and generates the correct `WHERE` clause
in the migration schema for both SQLite and Postgres. No raw SQL migration hooks
are needed.

## Verification

1. `go generate ./pkg/ent/` — succeeded, generated all ent client code
2. `go build ./pkg/ent/...` — compiled cleanly
3. `go build ./...` — full project compiles with no breakage
4. Partial index WHERE clause confirmed in `pkg/ent/migrate/schema.go`

## Design Decisions

- **No edges**: foreign keys (`project_id`, `default_agent_id`,
  `conversation_id`, `message_id`) are plain UUID columns, not ent edges.
  This keeps schemas independent and avoids modifying existing entities.
- **`default_agent_id` is UUID**: enforces that agent slugs cannot be stored,
  as required by the design.
- **Enum fields**: all categorical values use `field.Enum` with `.Values()`.
