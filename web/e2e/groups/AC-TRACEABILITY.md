# AC Traceability Table

Maps each acceptance criterion (1–22) from the design doc (§13) to the
E2E spec(s) or unit test(s) that verify it.

| AC | Description | Verified by |
|----|-------------|-------------|
| 1 | Capability-gated Create button (create cap drives it, not role) | `03-capability-gating.spec.ts` |
| 2 | Create with name-only → slug derived → detail page → creator is owner | `04-create-happy-path.spec.ts` |
| 3 | Duplicate slug inline error + focus; project: prefix rejected pre-submit | `05-create-errors.spec.ts` |
| 4 | Search/filter/pagination server-driven; URL round-trips; Previous/Next integrity | `01-list-search-filter-paginate.spec.ts` |
| 5 | Filtered-empty vs truly-empty distinct; empty state shows Create CTA only with cap | `02-list-states.spec.ts` |
| 6 | My groups lists effective (including nested) memberships | `06-my-groups-tab.spec.ts` |
| 7 | Edit: rename, description, labels, owner transfer, slug immutable, blank description | `07-edit-group.spec.ts` |
| 8 | Edit/Delete/Add/Remove affordances render iff matching \_capabilities action | `03-capability-gating.spec.ts` |
| 9 | project_agents: system-managed notice, zero mutation affordances | `08-project-agents-group.spec.ts` |
| 10 | Add user/group/agent; display names; duplicate → inline error | `09-members-add.spec.ts` |
| 11 | Cycle: add ancestor → cycle explanation inline, dialog open, no membership | `10-cycle-detection.spec.ts` |
| 12 | Quota: seeded limit → inline quota copy | `11-quota-exceeded.spec.ts` |
| 13 | Role hierarchy: admin adds member OK, adding owner denied | `12-role-hierarchy.spec.ts` |
| 14 | Sole-owner: disabled remove + tooltip; 2nd owner enables | `13-sole-owner.spec.ts` |
| 15 | Delete: typed-slug gate, success → list + toast | `14-delete-group.spec.ts` |
| 16 | Constraint-bearing delete → protection dialog + boundaries link | `15-constraint-delete.spec.ts` |
| 17 | Member remove security-review/lockout dialog | `16-member-remove-security-review.spec.ts` |
| 18 | Project settings member editor unchanged | `17-project-settings-regression.spec.ts`, `group-member-editor.test.ts` (unit) |
| 19 | Keyboard-only: create, add member, delete via keyboard | `18-keyboard-only.spec.ts` |
| 20 | axe: list, detail, all dialogs; light + dark; no serious/critical | `19-axe-accessibility.spec.ts` |
| 21 | Mobile 390×844: specs 1,4,9,14 re-run | `20-mobile.spec.ts` |
| 22 | typecheck/lint/test green | CI pipeline (`npm run typecheck && npm run lint && npm test && npm run test:e2e`) |
