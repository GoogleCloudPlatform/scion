# DEF-8/DEF-10 Code Convergence

**Date:** 2026-08-27
**Author:** dev-def8-convergence
**Branch:** scion/ca-msg-em6
**Base:** scion/messaging-v2 (ebf8cc27)

## Problem

Two code paths created direct conversations using incompatible identity keys:

- **Path A** (dual-write, `ResolveOrCreateDMConversation`): Used `dm:{sorted(idA,idB)}` (kind-free), nil ProjectID, zero participants.
- **Path B** (resolver, `resolveAgentDM`/`resolveEmailDM`): Used participant-scan with N+1 queries, empty external_ref, ProjectID = sender's project.

The two paths could never find each other's rows. Same pair could produce two conversation IDs.

## Changes

### 1. DMConversationKey (pkg/messages/dm_key.go) — NEW

Canonical key derivation for DM conversations:
- Format: `dm:<kind>:<uuid>:<kind>:<uuid>` with tokens sorted lexicographically
- kind ∈ {user, agent}, lowercase
- UUID normalised to canonical lowercase hex-with-hyphens before sorting
- Rejects malformed UUIDs and unknown kinds
- `ParseDMKey` for roundtrip parsing

### 2. SetDisplayName clobber fix (pkg/store/entadapter/conversation_store.go)

`UpsertConversationByExternalRef` previously set `SetDisplayName(conv.DisplayName)` unconditionally on the update branch, clobbering existing display names when upserting with an empty name. Fixed to skip when `conv.DisplayName` is empty.

### 3. resolve.go convergence

- `resolveAgentDM`: Now uses `DMConversationKey` + `UpsertConversationByExternalRef` + `ensureParticipant`. Project context used only for slug resolution — the DM itself has nil ProjectID (global).
- `resolveEmailDM`: Same pattern. Already global, now uses upsert instead of participant scan.
- Added `UpsertConversationByExternalRef` to `ResolutionStore` interface.
- Added `ensureParticipant` helper (handles `ErrAlreadyExists` gracefully).
- Deleted `findDirectConversation` (replaced by indexed upsert lookup).
- Deleted `createDirectConversation` (folded into upsert + ensure pattern).

### 4. Tests

- **Guard A:** Zero direct conversations with empty external_ref (floor ≥ 3 rows).
- **Guard B:** Every `dm:` row has exactly 2 active participants (floor ≥ 3 rows).
- **AC-DEF8-1:** Two sends to the same agent via @agent resolve to ONE conversation row.
- **AC-DEF8-2:** All direct conversations have nil ProjectID (closes DEF-10).
- **SetDisplayName test:** Upsert with empty display name preserves existing name.
- **DMConversationKey tests:** Roundtrip, ordering, rejection, case normalisation.

## What was NOT changed

- `DirectMessageExternalRef` in divergence.go — stays as-is for the legacy path.
- `ResolveOrCreateDMConversation` in conversation.go — continues with old format.
- No files outside scope (resolve.go, resolve_test.go, conversation_store.go, pkg/messages/).

## DEF-10 resolution

All direct conversations created through the resolve paths now have nil ProjectID (global DMs). The old code set `ProjectID = sender's project`, which was incorrect.
