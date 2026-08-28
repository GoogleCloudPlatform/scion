# CI: Conversation-Minting Guard

**Date:** 2026-08-28
**Agent:** ca-msg-em6 (dev-ci-guard)
**Branch:** `scion/ca-msg-em6-ci-guard`

## Summary

Lifted the conversation-minting CI guard from the tranche C branch
(`scion/ca-msg-em9-unify`, authored by ca-msg-em9) and applied it as a
standalone commit against `upstream/main`.

## What was done

1. **Created `hack/check-conversation-upsert-guard.sh`** — a textual guard that
   ensures conversation-minting methods and raw SQL INSERTs are confined to
   `pkg/messaging/` and `pkg/store/`.

2. **Added Makefile target** `check-conversation-upsert-guard` and wired it into
   both `ci` and `ci-full` dependency lists.

3. **Added CI workflow step** in `.github/workflows/ci.yml`.

## Modifications from em9's original

| Change | Rationale |
|--------|-----------|
| Extended Check 1 grep to include `AddParticipant` | AddParticipant modifies the participant listing index; unguarded calls outside the resolve flow corrupt conversation visibility |
| Removed `webchannel_store` exemption from Check 3 | That exemption is for dual-write paths that don't exist on main yet — they arrive with tranche C |
| Updated severity framing from "authorization"/"auth bypass" to "listing-index integrity" | The participant table is a derived listing index, not the access authority. Exception noted for unknown conversation kinds where requireParticipant acts as ACL |

## Verification results

| Step | Expected | Actual |
|------|----------|--------|
| Guard on clean branch | exit 0 | exit 0 |
| Positive control: `AddParticipant` probe in `pkg/hub/` | exit 1 | exit 1 |
| Positive control: `UpsertConversationByExternalRef` probe in `pkg/hub/` | exit 1 | exit 1 |
| `make compat-literals check-authz-guards check-conversation-upsert-guard` | exit 0 | exit 0 |
