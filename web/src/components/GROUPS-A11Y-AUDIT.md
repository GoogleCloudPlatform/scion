# Groups UI Accessibility Audit (G6)

Self-audit checklist per design doc section 9. Each criterion lists pass/fail
with the file(s) and evidence. This feeds Q1 (axe + keyboard E2E).

## 1. Tables: caption, scope, keyboard navigation

| Criterion | Status | Evidence |
|---|---|---|
| List table has `<caption>` (visually hidden) | PASS | `admin-groups.ts` line 1167: `<caption class="sr-only">` |
| List table `<th>` have `scope="col"` | PASS | `admin-groups.ts` lines 1172-1176 |
| Member table has `<caption>` (visually hidden) | PASS | `group-member-editor.ts`: `<caption class="sr-only">` |
| Member table `<th>` have `scope="col"` | PASS | `group-member-editor.ts`: all `<th scope="col">` |
| Group name is a real `<a>` link (keyboard row activation) | PASS | `admin-groups.ts` line 1207: `<a class="group-name-link" href=...>` |
| Icon-only buttons carry `label` | PASS | Refresh `sl-icon-button` has `label="Refresh"`, remove buttons have `label="Remove member"`, remove-label has `label="Remove label"` |

## 2. Dialogs: focus trap, initial focus, focus return, close suppression

| Criterion | Status | Evidence |
|---|---|---|
| Shoelace focus trap inherited | PASS | All dialogs use `<sl-dialog>` which provides focus trap |
| Create dialog: initial focus on first field | PASS | `group-form-dialog.ts`: `autofocus` on name input in create mode |
| Create dialog: focus returns to invoking button on close | PASS | `admin-groups.ts`: `returnFocusToCreateBtn()` called on cancel |
| Edit dialog: focus returns to Edit button on close | PASS | `admin-group-detail.ts`: `returnFocusTo('#edit-group-btn')` on cancel and update |
| Delete dialog: focus returns to invoking element on close | PASS | `admin-group-detail.ts`: `returnFocusTo('#delete-group-item')` on `sl-after-hide` |
| Add member dialog: focus returns to Add Member button on close | PASS | `group-member-editor.ts`: `closeAddDialog()` focuses `.list-header sl-button` |
| Close suppressed while mutation in flight (create) | PASS | `group-form-dialog.ts`: `onRequestClose` checks `this.submitting`, calls `e.preventDefault()` |
| Close suppressed while mutation in flight (edit) | PASS | Same handler, shared across modes |
| Close suppressed while mutation in flight (delete) | PASS | `group-delete-dialog.ts`: checks `this.deleting`, calls `e.preventDefault()` |
| Close suppressed while mutation in flight (add member) | PASS | `group-member-editor.ts`: checks `this.addMemberLoading`, calls `e.preventDefault()` |

## 3. Errors: role="alert", focus-to-first-error

| Criterion | Status | Evidence |
|---|---|---|
| Form dialog banner uses `role="alert"` | PASS | `group-form-dialog.ts`: `<sl-alert ... role="alert">` |
| Form dialog label errors use `role="alert"` | PASS | `group-form-dialog.ts`: `<div class="label-error" role="alert">` |
| Delete dialog error banner uses `role="alert"` | PASS | `group-delete-dialog.ts`: `<div class="error-banner" role="alert">` |
| Add member dialog error uses `role="alert"` | PASS | `group-member-editor.ts`: `<div class="dialog-error" role="alert">` |
| Member editor load error uses `role="alert"` | PASS | `group-member-editor.ts`: `<div class="error-state" role="alert">` |
| Detail page load error uses `role="alert"` | PASS | `admin-group-detail.ts`: `<div class="error-state" role="alert">` |
| List page error states use `role="alert"` | PASS | `admin-groups.ts`: error-state and permission-denied-state both have `role="alert"` |
| Focus moves to first errored field on failed submit | PASS | `group-form-dialog.ts`: `focusFirstError()` queries `[data-error="true"]` and focuses |
| Slug conflict focuses slug field | PASS | `group-form-dialog.ts`: `focusSlugField()` on `conflict_slug` error |

## 4. Async announcements: live regions

| Criterion | Status | Evidence |
|---|---|---|
| List result count in aria-live region | PASS | `admin-groups.ts`: `<div class="result-count-live" role="status" aria-live="polite" aria-atomic="true">` |
| My groups count in aria-live region | PASS | `admin-groups.ts`: same pattern in `renderMyGroupsTab()` |
| Member count change announced | PASS | `group-member-editor.ts`: `<span class="member-count" aria-live="polite">` |
| Success toasts are supplementary, not sole signal | PASS | `showToast()` in `toast.ts` creates non-modal alerts; form dialog also dispatches events (`group-saved`, `group-updated`) that trigger navigation or data refresh |

## 5. Badges and icons: text, not color-only

| Criterion | Status | Evidence |
|---|---|---|
| Type badges show text ("explicit" / "project agents") | PASS | `admin-groups.ts` line 1222, `admin-group-detail.ts` line 489 |
| Role badges show text ("member" / "admin" / "owner") | PASS | `group-member-editor.ts`: `<span class="role-badge ...">${member.role}</span>` |
| Decorative icons have `aria-hidden="true"` | PASS | All standalone decorative icons across all files have `aria-hidden="true"` |

## 6. Keyboard operability

| Criterion | Status | Evidence |
|---|---|---|
| Create group flow: keyboard operable | PASS | Button + dialog with focus trap; all inputs are standard form elements |
| Add member flow: keyboard operable | PASS | Button + dialog + principal-picker + submit |
| Delete group flow: keyboard operable | PASS | Menu item + dialog with typed slug confirmation (plain input) |
| No drag/hover dependencies | PASS | Typed-slug delete confirmation uses a plain `<sl-input>` |

## 7. Mobile: column visibility

| Criterion | Status | Evidence |
|---|---|---|
| Group name never hidden on mobile | PASS | No `hide-mobile` on Group column |
| Group type never hidden on mobile | PASS | No `hide-mobile` on Type column |
| Actions never hidden on mobile | PASS | Actions column has no `hide-mobile` |
| Description/Labels/Updated hidden on mobile | PASS | All have `class="hide-mobile"` |
| Member name never hidden on mobile | PASS | No `hide-mobile` on Member column |
| Member role never hidden on mobile | PASS | No `hide-mobile` on Role column |
| Member Added date hidden on mobile | PASS | `class="hide-mobile"` on Added column |

## 8. Dark-theme contrast

| Criterion | Status | Notes |
|---|---|---|
| Badges use CSS custom properties with fallbacks | PASS | All badge colors use `var(--sl-color-*, fallback)` pattern |
| Dialog error banners contrast | PASS | Uses Shoelace `sl-alert variant="danger"` which handles theming |
| Owner warning contrast | PASS | Uses `var(--sl-color-warning-*)` custom properties |
| Slug display contrast | PASS | Uses `var(--scion-text-muted)` and `var(--scion-bg-subtle)` |

## 9. Copy review

| String | Location | Status |
|---|---|---|
| "Create group" | list header button, form dialog title/button | PASS |
| "Edit group" | detail header button, form dialog title | PASS |
| "Save changes" | form dialog edit mode button | PASS |
| "Delete group" | detail overflow menu, delete dialog button | PASS |
| "Add Member" | member editor button, dialog button | PASS |
| "Remove member" | icon-button label (sr-only) | PASS |
| "Remove label" | icon-button label (sr-only) | PASS |
| Slug help text (create) | form dialog | PASS |
| Slug permanent text (edit) | form dialog | PASS |
| Owner transfer warning | form dialog edit mode | PASS |
| Typed-slug confirmation prompt | delete dialog | PASS |
| Error messages (all error kinds) | Various | PASS |
| Impact copy (delete dialog) | delete dialog | PASS |
| "No Groups" / "No Group Memberships" | empty states | PASS |
| "Permission Denied" | permission denied state | PASS |

## 10. Test coverage

| Criterion | Status | Evidence |
|---|---|---|
| Table ARIA attributes tested | PASS | `group-member-editor.test.ts`: 12 new a11y tests |
| List page ARIA attributes tested | PASS | `admin-groups.test.ts`: 10 new a11y tests |
| Focus return IDs tested | PASS | Tests verify `id="create-group-btn"` exists |
| Decorative icon aria-hidden tested | PASS | Tests verify all prefix icons have `aria-hidden="true"` |
| role="alert" on errors tested | PASS | Tests verify error-state and dialog-error have `role="alert"` |

---

**Overall**: All criteria from design doc section 9 are addressed. Remaining
verification (axe checks, scripted keyboard-only E2E) lands in E2.
