# members-multirole P2: nil membershipService guard (GoogleCloudPlatform/scion#2320)

**Branch**: `scion/members-multirole-p2`. Follows an accepted Gemini review
comment (medium) on upstream PR GoogleCloudPlatform/scion#2320.

## Change

- `memberListCapabilities` (`pkg/hub/handlers_project_members.go`) called
  `s.membershipService.ComputeCapabilities` with no nil check, while every
  sibling members handler guards `s.membershipService`. It now returns nil
  when the service is nil. Capabilities are advisory, so the list still
  returns 200 with `_capabilities` omitted rather than an HTTP error.

## Test

- `TestProjectMembers_List_NilMembershipServiceOmitsCapabilities` hits
  `GET /api/v1/projects/{id}/members` as the project owner with a nil
  service and asserts 200, items present, and nil capabilities; it also
  calls `memberListCapabilities` directly with a user identity in context.
- Revert proof: with the guard removed, the request panics (recovered by the
  middleware as a nil pointer dereference, so 500) and the test fails.

## Notes

- Only gate-scoped targeted tests were run (broker throttle).
