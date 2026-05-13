# Unify Allow List & Invite Management UX

**Date:** 2026-05-13
**Agent:** fix-invite-ux
**Branch:** scion/fix-invite-ux

## Summary

Unified the previously separate Allow List and Invites tabs in the admin Users page into a single "Members" view with inline invite management. The flow is now: add member → generate their invite → share the link.

## Changes

### Backend
- Added `AllowListEntryWithInvite` model that enriches allow-list entries with joined invite code details
- Added `UpdateAllowListEntryInviteID` store method to link invites to allow-list entries at creation time
- Added `ListAllowListEntriesWithInvites` store method using LEFT JOIN for efficient enriched queries
- Extended `InviteCreateRequest` with optional `Email` field — when provided, the invite is linked to the matching allow-list entry
- Updated the allow-list GET endpoint to return enriched data with inline invite status

### Frontend
- Renamed "Allow List" tab → "Members", "Invites" tab → "All Invites"
- Each member row now shows inline invite status (active/expired/revoked/exhausted)
- Per-row dropdown with: Generate Invite, Revoke Invite, Remove
- Create-invite dialog shows contextual header when generating for a specific member
- After adding a new member, the invite dialog opens automatically pre-filled

### Testing
- 7 new tests in `pkg/store/sqlite/allow_list_invite_test.go` covering:
  - UpdateAllowListEntryInviteID: success, case-insensitive, not-found
  - ListAllowListEntriesWithInvites: no invite, with linked invite, revoked, mixed entries

## Design Decisions

1. **LEFT JOIN approach**: Rather than N+1 queries to load invite details per row, the enriched list uses a single SQL LEFT JOIN between `allow_list` and `invite_codes`.

2. **Best-effort linking**: When creating an invite with an email, the allow-list entry update is best-effort — if the email isn't on the allow list, the invite is still created. This avoids coupling the two operations.

3. **Kept "All Invites" tab**: Rather than removing the invites tab entirely, it's preserved as a secondary view for bulk invite management and viewing invites not linked to specific members.

4. **Auto-open invite dialog after add**: When a new member is added, the invite dialog opens immediately. This gives the "add member and generate invite in one flow" experience.
