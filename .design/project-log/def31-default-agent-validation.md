# DEF-31: Fix cross-project agent routing via unvalidated defaultAgent

**Date:** 2026-08-28
**Branch:** `scion/ca-msg-em6-def31`
**Files changed:** `pkg/hub/handlers_chat_v2.go`, `pkg/hub/handlers_chat_v2_test.go`

## Problem

The `defaultAgent` field on topic CreateTopic and UpdateTopic request bodies was
stored without any validation, while sibling fields (like `name`) were validated
four ways (required, trimmed, <=100 runes, regexp). This created a three-link
defect chain:

1. **Unvalidated ingress:** `body.DefaultAgent` stored as-is at CreateTopic
   (line ~451) and UpdateTopic (lines ~544, 575-576) with zero validation.

2. **Unscoped resolver fallback:** The default-agent resolver at send time
   (line ~932-940) did a two-step lookup:
   - Step 1: `GetAgentBySlug(projectID, raw)` — project-scoped, filters deleted
   - Step 2 (fallback): `GetAgent(raw)` — bare primary-key fetch with NO project
     filter and NO `deleted_at` filter

3. **Cross-project binding:** A raw UUID naming an agent in another project, or a
   soft-deleted agent, would bind successfully via step 2, defeating
   `ClearTopicDefaultAgent` (which scrubs bindings on agent deletion).

## Severity

The participant table is a derived listing index, not the access authority.
Auth for DM conversations is key-derived (checks kind AND id via ParseDMKey on
external_ref). Auth for group conversations is project membership. An unguarded
defaultAgent corrupts **listings** (conversation appears in wrong sidebar), NOT
access (cannot read or post). This is NOT an authorization bypass.

Exception: the `default` branch in resolve.go for unknown conversation kinds
falls back to `requireParticipant`, where the participant table IS the authority.
No such kind exists today. Accurate severity: "not an ACL today; an ACL for any
future conversation kind someone forgets to case."

## Fix

### (a) Lookup fix (load-bearing)

After the `GetAgent` fallback returns in the resolver, added project-ID and
deleted-at checks:

```go
if defaultAgent.ProjectID != projectID || !defaultAgent.DeletedAt.IsZero() {
    defaultAgent = nil
}
```

This does NOT change GetAgent's global signature — the constraint is at the
call site only.

### (b) Ingress validation (makes failure legible)

Added `validateDefaultAgent` helper method that performs the same two-step
lookup (slug first, then UUID) with project-scoping and deleted-at checks.
Called from both:
- `handleCreateThread` — validates `body.DefaultAgent` before creating the topic
- `handleTopicPatch` — validates `*body.DefaultAgent` before updating (clearing
  via empty string is always allowed)

Both request body structs were audited:
- **CreateTopic body:** `Name` (validated 4 ways), `DefaultAgent` (now validated)
- **UpdateTopic body:** `Name` (validated 4 ways when provided), `DefaultAgent` (now validated)

No other unvalidated fields exist on either struct.

## Tests

Five tests added per specification:

1. **TestDEF31_ForeignProjectUUID_Rejected** — agent in project B rejected as
   defaultAgent on topic in project A, for both POST (create) and PATCH (update).

2. **TestDEF31_SoftDeletedAgent_Rejected** — soft-deleted agent rejected as
   defaultAgent, for both POST and PATCH.

3. **TestDEF31_Rebinding_AfterSoftDelete** — binds agent, soft-deletes it,
   confirms ClearTopicDefaultAgent scrubs the binding, then tests that even
   manually re-setting the binding is caught by validateDefaultAgent.

4. **TestDEF31_PairedPositives** — valid slug binds, valid same-project UUID
   binds, clearing via empty string works. Proves the validator accepts valid
   inputs (not just reject everything).

5. **TestDEF31_MutationTest_LookupScoping** — structural mutation test:
   exercises validateDefaultAgent directly with foreign-project UUID and
   soft-deleted UUID, asserting specific error messages. If the project-ID or
   deleted-at guards are removed, these assertions fail with diagnostic messages
   naming the defect — not a panic or compile error.

## Send-path resolver tests (added in review round 2)

The initial tests only exercised `validateDefaultAgent` (ingress helper). The
reviewer correctly identified that the load-bearing resolver guard at the send
path had zero test coverage. Three send-path tests were added:

6. **TestDEF31_SendPath_ForeignProjectAgent_NotRouted** — writes a topic with a
   foreign-project agent UUID directly via `wcs.CreateTopic` (bypassing ingress),
   POSTs a message, asserts `type=chat` (no agent routing).

7. **TestDEF31_SendPath_SoftDeletedAgent_NotRouted** — same pattern with a
   soft-deleted same-project agent UUID.

8. **TestDEF31_SendPath_ValidAgent_StillRoutes** — paired positive: a valid
   same-project agent still routes (`type=instruction`). Without this, deleting
   the entire routing branch passes all tests.

**Mutation result:** Removed the resolver guard (lines 995-1001), ran send-path
tests — both negative tests FAILED with assertions naming the wrong-project/
deleted-agent routing. Positive test still passed. Restored guard — all 11 tests
passed.

## Whitespace fix (added in review round 2)

Whitespace-only `defaultAgent` (`"   "`) behaved differently on the two
endpoints: CreateThread trimmed inside the `!= ""` check (so `"   "` → `""`
triggered a confusing validation error), while TopicPatch trimmed first (treating
it as a clear). Fixed CreateThread to trim before the empty check, matching PATCH
behavior.

## Pre-existing test failure

`TestTemplateResource_UATConfinement/global_template_is_still_not_confined_(unchanged)`
fails on upstream/main independently of these changes.
