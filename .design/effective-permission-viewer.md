# Effective Permission Viewer — Design Backlog

## Status

**Backlog** — not yet implemented.

## Motivation

The previous "Effective access composition" section in the user/agent
Effective Roles dialog was removed because the backend only returns a
system-scope `activeBindingCount`.  Repeating that count as if it were a
permission composition was misleading.

A dedicated Effective Permission Viewer should explain exactly what a
principal can do and why, replacing the removed section with accurate,
actionable information.

## Scope

A standalone, reusable view or tool that can later be embedded in or
linked from the Effective Roles dialog, admin user detail, and agent
detail pages.

## Required Capabilities

The viewer should explain:

1. **Active grants** — which role bindings are currently active for the
   principal, with provenance (direct vs group-derived).
2. **Contributing bindings** — which bindings contribute each permission
   in the effective set.
3. **Scope** — whether a grant applies system-wide or is scoped to a
   specific project.
4. **Access-constraint reductions** — which access constraints (access
   boundaries) apply to the principal and which permissions they remove.
5. **Credential / context restrictions** — credential scope, principal
   status, delegation ceiling, and other intrinsic restrictions that
   further reduce the effective set.
6. **Resulting permission set** — the final set of permissions after all
   layers are applied, with per-permission denial reasons when a
   permission is removed.

## Backend Prerequisites

- Per-permission effective-access computation (not just
  `activeBindingCount`).
- Scope-aware evaluation: project-scoped grants should be evaluable
  in project context, not only system scope.
- Denied-permission reasons with boundary/restriction attribution.

## UI Guidelines

- Reuse the layered visualization pattern (potential → constraints →
  restrictions → effective) but only when backed by real per-permission
  data.
- Show "removed by both" for overlapping boundary removals; never use
  "priority", "override", or "winner" terminology.
- Handle redacted boundaries gracefully: show "Access constraint
  (details unavailable)" without attempting a secondary name fetch.
- The viewer should be a self-contained component that receives data
  via properties, not by fetching directly — keep the data layer in the
  parent page.

## References

- Removed composition code: commit that introduced this design note.
- `authorization-layer-stack.ts` — retained in the codebase as a
  reference for the layered visualization pattern (currently unused).
- `effective-access-boundary-notice.ts` — the boundary-count notice
  component, still in use.
