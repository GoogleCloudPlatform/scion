# Resource Clone & Delete (Reduced Hub Resource Management)

**Status:** Reviewed — all open questions resolved (see §3.1); ready for implementation
**Created:** 2026-06-02
**Author:** Agent (template-harness-refactor)
**Related:** [hub-template-admin.md](./hub-template-admin.md) (parent — full admin view), [resource-import-refactor.md](./resource-import-refactor.md) (import slice, now built), [grove-level-templates.md](./grove-level-templates.md), [agnostic-template-design.md](./agnostic-template-design.md)

---

## 1. Overview

`hub-template-admin.md` proposed a full hub admin template-management page: a
dedicated `/admin/templates` list with filters/sorting/pagination and a row action
menu offering View/Edit, **Clone**, Lock/Unlock, Archive, and **Delete**, plus
hub-level import. Since that draft, two things changed the ground under it:

1. **The import slice is fully built** (`resource-import-refactor.md`, Phases 0–4):
   a unified `POST /api/v1/resources/import` endpoint, a shared
   `<scion-resource-import>` web component mounted in both Project Settings →
   Resources and the Hub Resources page, streaming per-resource progress, and a
   parallelized import path. Import is **done** and is no longer part of this work.
2. **A shared, kind-generic resource UI already exists.** `<scion-resource-list>`
   (`web/src/components/shared/resource-list.ts`) renders templates *and*
   harness-configs identically in both the project and hub surfaces — but it is
   **read-only** today (lists + links to the detail/editor page; explicitly "does
   not handle import/creation").

This document is the **reduced adaptation** of `hub-template-admin.md` against that
updated current state. It drops the standalone admin page, filters, sorting,
pagination, lock/unlock, and archive, and keeps only the two operations the user
asked for — **Clone** and **Delete** — wired into the resource list that already
ships in both surfaces. It also folds in a requested **verification that
re-importing from the same URL pulls fresh content** (§5), which turns out to
already hold and just needs a regression test to lock it in.

### Why "reduced"

The full admin page in `hub-template-admin.md` assumed no shared resource UI and no
import. Both now exist. Building a separate `/admin/templates` page would duplicate
the list that `<scion-resource-list>` already renders in two places. Adding Clone and
Delete *to that shared list* gives the two highest-value management actions in both
the project and hub Resources views with no new page, no new list, and a small,
well-contained backend delta.

---

## 2. Current State (as built)

### 2.1 Backend — what already exists

| Operation | Endpoint | Handler | Notes |
|-----------|----------|---------|-------|
| Delete template | `DELETE /api/v1/templates/{id}?deleteFiles=true&force=true` | `deleteTemplateV2` (`template_handlers.go:495`) | Deletes DB record; `deleteFiles=true` also `DeletePrefix`es storage; `force=true` required to delete a `Locked` template (see §2.4). |
| Clone template | `POST /api/v1/templates/{id}/clone` | `handleTemplateClone` (`template_handlers.go:693`) | Copies files via `stor.Copy`, sets `BaseTemplate = source.ID`, **destination scope/scopeId/name come from the request body** (`CloneTemplateRequest`) — so it can already clone across scopes. |
| Delete harness-config | `DELETE /api/v1/harness-configs/{id}?deleteFiles=true` | `deleteHarnessConfig` (`harness_config_handlers.go:~370`) | Deletes record; `deleteFiles=true` removes storage. Routed via `handleHarnessConfigByID` → CRUD switch (`harness_config_handlers.go:260`). |

**Two backend deltas this work introduces (details in §4):**
- **No harness-config clone endpoint.** The `action` switch in `handleHarnessConfigByID`
  (`harness_config_handlers.go:230`) handles `upload`, `finalize`, `download`,
  `files/…` — but no `clone`. Templates get clone; harness-configs don't.
- **No resource-level authz on delete/clone.** See §2.5.

### 2.2 Frontend — what already exists

- `<scion-resource-list>` (`resource-list.ts`, 251 lines) — shared, read-only list
  used by **both** Project Settings → Resources and the Hub Resources page
  (`settings.ts`). No row actions today.
- `<scion-resource-import>` (`resource-import.ts`) — shared import form, mounted in
  both surfaces. Emits `resource-imported` so the host refreshes the list.
- Resource detail/editor page — file browser + inline editor, reachable from each
  list row via `detailBasePath`.

So the surfaces, the shared list, and the import affordance are all in place. What's
missing is **row-level Clone/Delete actions** (and, for cross-scope clone, a
"clone from global" affordance in the project view — §4.2).

### 2.3 Re-import freshness — current behavior (verified)

Traced end-to-end for the "re-import the same URL" case:

1. **Fetch layer** — `FetchRemoteTemplate` (`pkg/config/remote_templates.go:160`)
   computes its cache key from the **URL only** (`generateCacheKey(uri)`), then
   `os.RemoveAll(templateCachePath)` (`remote_templates.go:180`) **before**
   re-downloading. Re-importing the same URL wipes the prior cached copy and pulls a
   fresh tarball/checkout every time — there is no stale-cache reuse.
2. **Sync layer** — the import loop calls `ResourceStore.Bootstrap(..., force=true)`
   (`pkg/hub/resource_import.go:390`). With `force=true`, Bootstrap **skips the
   unchanged-hash short-circuit** (`resource_store.go:179`), re-uploads all files,
   and calls `reconcileResourceStorage` to **drop objects no longer in the manifest**
   so files removed upstream don't linger. It recomputes the content hash and flips
   the record back to `active`.

**Conclusion: re-importing from the same URL already pulls fresh content** — fresh
bytes at the fetch layer, forced re-upload + stale-file pruning at the sync layer.
This is a behavior to *protect with a test*, not a bug to fix (§5).

### 2.4 The `Locked` flag is latent (never set today)

`Locked` (`pkg/store/models.go:422` "Prevent modifications (global templates)") is
**read and enforced** in many places — template/harness-config update, PATCH, file
edit, and delete all reject when `Locked` is set
(`template_handlers.go:409,449,510`; `harness_config_handlers.go:292,330,388`;
`template_file_handlers.go:308,480,610`) — and it is persisted in SQLite. **But no
non-test code path ever sets `Locked = true`.** The CRUD/PATCH handlers only
*preserve* it (`template.Locked = existing.Locked`, `template_handlers.go:424`); the
bootstrap/import path doesn't set it; there is no lock/unlock endpoint. The only
assignments to `true` are in tests (`template_file_handlers_test.go:331,413,563`).

**Implication for this work:** in practice a template/harness-config is never locked,
so the delete path's locked branch is currently unreachable. We keep delete honoring
the flag (and offer a force fallback, §4.2) as cheap, correct insurance for if/when a
lock-setter is added — but we add **no** lock/unlock UI, and the locked-state UX is
not a focus. (This is the answer to the reviewer's "how do templates get locked?" —
today, they don't.)

### 2.5 Authz gap on delete/clone (to be closed)

`/api/v1/*` is wrapped by the global auth **middleware** (`server.go:1880`
`applyMiddleware`), which establishes *authentication* (valid session/bearer). But
the existing `deleteTemplateV2`, `handleTemplateClone`, and `deleteHarnessConfig`
handlers contain **no resource-level `CheckAccess`** — so any authenticated user can
call them directly, regardless of scope or role. By contrast the newer import
endpoint (`handleResourcesImport`) *does* enforce authz (hub-admin for global scope,
project capability for project scope; covered by
`resource_import_handler_test.go`). This work closes that gap (§4.3).

---

## 3. Goals & Non-Goals

### Goals
- Add **Clone** and **Delete** row actions to the shared `<scion-resource-list>`, so
  both Project Settings → Resources and Hub Resources get them with one change.
- Add a **harness-config clone** endpoint mirroring the template clone, so the list's
  Clone action works for both kinds.
- Support **cloning a global resource into a project** from the project Resources view
  (cross-scope clone, global → project). The clone endpoints already accept a
  destination scope/scopeId, so this is mostly UI plus the new harness-config clone.
- **Harden authz**: add `CheckAccess` to the delete and clone handlers, matching the
  import endpoint's policy (hub-admin for global-scoped; project capability for
  project-scoped).
- A **destructive-action confirmation** for Delete (with a "delete stored files"
  checkbox **defaulting on**) and a small **name/destination dialog** for Clone.
- **Lock in re-import freshness** with regression tests (§5).

### Non-Goals
- The standalone `/admin/templates` page, filters, sorting, pagination — dropped; the
  shared list already covers listing.
- **Lock/Unlock** and **Archive** actions/UI. Delete still *honors* the `Locked`
  flag, but per §2.4 nothing sets it today, so there is no UI to toggle it.
- A referenced-by / in-use guard on delete — **hard delete** is retained (§3.1 Q5).
- Bulk actions and template usage indicators.
- Any change to the import pipeline or the `ResourceStore` core.

### 3.1 Resolved decisions (review with project owner)

- **Q1 → Add authz.** Fold resource-level `CheckAccess` into delete + clone (both
  kinds), matching the import endpoint policy (§4.3). Closes the gap in §2.5.
- **Q2 → `deleteFiles` default ON.** The delete dialog's "Also delete stored files"
  checkbox defaults checked (clean delete that reclaims storage).
- **Q3 → Locked is latent; no lock UI.** Investigated: no non-test code sets `Locked`
  (§2.4). Keep delete honoring the flag with a force-delete confirm fallback; add no
  lock/unlock UI.
- **Q4 → Clone from global in the project view.** The project Resources view offers
  cloning a **global** resource down into the current project (§4.2). Same-scope clone
  (project→project, global→global) is also supported.
- **Q5 → Hard delete.** No referenced-by/in-use guard; delete is immediate with only
  the generic "cannot be undone" warning.

---

## 4. Design

### 4.1 Backend: harness-config clone + shared clone request

Add a `clone` action to `handleHarnessConfigByID` (`harness_config_handlers.go:230`):

```go
case "clone":
    s.handleHarnessConfigClone(w, r, hcID)
```

`handleHarnessConfigClone` mirrors `handleTemplateClone` (`template_handlers.go:693`):

1. `POST` only; load the source via `GetHarnessConfig`.
2. Read `{ name, scope, scopeId, visibility }` (reuse the `CloneTemplateRequest`
   shape; harness-configs also carry `Harness`, copied from the source).
3. Build a new record with a fresh UUID, `Slug = Slugify(name)`, copying
   `Harness`/`Description`/`Config`, **destination scope/scopeId from the request**
   (default to source scope when omitted), status `pending`.
4. Generate the clone's storage path and `stor.Copy` each source file to it.
5. Persist, set status `active`, return the new record.

Endpoint: `POST /api/v1/harness-configs/{id}/clone` — dispatches through the existing
`action` switch, no route-table change.

> **Delete needs no new endpoint** — `deleteTemplateV2` and `deleteHarnessConfig`
> already accept `deleteFiles` (templates also `force`). Only authz is added (§4.3).

> **Cross-scope clone needs no new endpoint** — `handleTemplateClone` already takes
> the destination scope/scopeId from the body; the new harness-config clone does the
> same. The "clone from global into project" feature is the UI in §4.2 calling these
> with `scope=project, scopeId={projectId}` against a global-scoped source.

### 4.2 Frontend: Clone + Delete on the shared list, and clone-from-global

Add an actions affordance to each row in `<scion-resource-list>` — an `sl-dropdown`
(or trailing icon buttons) with **Clone** and **Delete**. Both surfaces inherit it.

New component state: `cloneTarget`, `deleteTarget`, `cloneFromGlobalOpen`,
`actionInProgress`, `actionError`.

**Delete flow:**
- Click Delete → confirmation dialog (`sl-dialog`, `danger` confirm button).
- Shows name + kind + scope, an **"Also delete stored files" checkbox (checked by
  default**, Q2), and an irreversible-action warning. No referenced-by check (Q5).
- Confirm → `DELETE /api/v1/{templates|harness-configs}/{id}?deleteFiles={checked}`.
  On `204`, remove the row locally (or re-fetch) and emit `resource-changed`.
- **Locked fallback (latent, §2.4):** if the response is the locked-template
  validation error, surface it and offer a "Force delete" confirm that retries with
  `&force=true`. In practice this branch is unreachable today, but it's cheap and
  correct. (Harness-configs have no lock concept.)

**Clone flow (same-scope):**
- Click Clone → dialog prompting for the **new name** (prefilled `"{name}-copy"`);
  destination defaults to the current list's scope/scopeId.
- Confirm → `POST /api/v1/{templates|harness-configs}/{id}/clone` with
  `{ name, scope, scopeId }`. On success, re-fetch and emit `resource-changed`.

**Clone-from-global (project view only, Q4):**
- The project Resources view gains a **"Clone from global"** affordance (a button
  near the list / import form). It opens a picker listing **global** resources of the
  current `kind` (via the existing list API with `scope=global`).
- Selecting one opens the same clone dialog with the **destination fixed to the
  current project** (`scope=project, scopeId={projectId}`) and a prefilled name.
- Confirm → clone endpoint with the global source id and the project destination. The
  cloned copy then appears in the project list (`BaseTemplate` tracks the global
  source for templates).
- This is the cross-scope direction the owner asked for (pull a shared global resource
  down into a project to customize it). The hub (global) view keeps same-scope clone
  only.

Endpoint/path selection keys off the existing `kind` property
(`'template' | 'harness-config'`).

### 4.3 Authorization (Q1)

Add `authzService.CheckAccess` to the delete and clone handlers, matching
`handleResourcesImport`:

- **Global-scoped resource** (delete/clone of a global template or harness-config):
  require **hub-admin** (the admin bypass / explicit hub-wide policy), the same check
  the global import path uses.
- **Project-scoped resource:** require the caller's **project capability** for the
  mutating action (mirror the per-project import authz via the shared
  `authorizeProjectImport`-style helper) — `ActionDelete` for delete, `ActionCreate`
  for clone.
- **Clone specifically** touches two scopes: it **reads** the source and **creates**
  at the destination. Enforce read on the source's scope **and** create on the
  destination scope. For clone-from-global → project: the source is a global resource
  (world-readable to authenticated users for global resources, consistent with the
  Hub Resources view) and the destination requires project create capability.

This makes delete/clone consistent with import and removes the §2.5 gap. UI gating
(admin-only Hub Resources route) stays as defense-in-depth, but the backend is now
authoritative.

---

## 5. Re-import freshness: verify & lock in

Per §2.3 this **already works**. Add regression tests rather than a fix:

- **`pkg/hub` integration test** (alongside `resource_import_handler_test.go`):
  1. Import a single-resource workspace dir (file `home/a.txt` = `"v1"`).
  2. Mutate the source: change `a.txt` → `"v2"`, add `b.txt`, delete an existing file.
  3. Re-import the **same source**.
  4. Assert the stored manifest reflects `v2`, includes `b.txt`, and **no longer
     includes** the deleted file (exercises `reconcileResourceStorage`), and that the
     record's `ContentHash` changed and status is `active`.
- **Remote-cache test** (`pkg/config`): assert `FetchRemoteTemplate` re-fetches for
  the same URL — the cache dir is wiped/rewritten on the second call (guards the
  `RemoveAll` + URL-keyed cache behavior at `remote_templates.go:175–180`).

Both lock down the guarantee: "re-importing the same URL pulls fresh content,
including removals."

---

## 6. Phases

### Phase 1 — Backend: harness-config clone + authz hardening
- Add `handleHarnessConfigClone` + the `clone` action case; mirror template clone,
  honoring destination scope/scopeId from the body.
- Add `CheckAccess` to `deleteTemplateV2`, `handleTemplateClone`,
  `deleteHarnessConfig`, and the new harness-config clone (§4.3).
- Tests: clone copies files / sets new slug+scope / independent record; authz allows
  admin + project-capable callers and 403s others (mirror the import handler tests).

### Phase 2 — Frontend: Clone & Delete on the shared list + clone-from-global
- Add row actions, delete-confirm dialog (`deleteFiles` checked by default + force
  retry for the latent locked case), and the same-scope clone dialog to
  `<scion-resource-list>`; wire both kinds; emit `resource-changed`.
- Add the project-view "Clone from global" picker → clone into the current project.
- Verify in both project and hub surfaces.

### Phase 3 — Re-import freshness regression tests
- Add the `pkg/hub` re-import-mutation test and the `pkg/config` cache-refetch test
  (§5). No production code change expected.

---

## 7. Key Files

| Area | File |
|------|------|
| Template delete / clone (reuse + add authz) | `pkg/hub/template_handlers.go` (`deleteTemplateV2:495`, `handleTemplateClone:693`) |
| Harness-config delete (add authz) + **new clone** | `pkg/hub/harness_config_handlers.go` (action switch `:230`, CRUD `:251`) |
| Authz reference (import) | `pkg/hub/handlers.go` (`handleResourcesImport`), `pkg/hub/resource_import_handler_test.go` |
| Re-import force-sync (verify) | `pkg/hub/resource_import.go` (`:390`), `pkg/hub/resource_store.go` (`Bootstrap:121`, reconcile) |
| Remote fetch cache (verify) | `pkg/config/remote_templates.go` (`FetchRemoteTemplate:160`, `RemoveAll:180`) |
| `Locked` model + enforcement (latent) | `pkg/store/models.go:422,517`; enforcement sites in `template_handlers.go`, `harness_config_handlers.go`, `template_file_handlers.go` |
| Shared list — **add actions** | `web/src/components/shared/resource-list.ts` |
| Host surfaces (inherit actions; project view adds clone-from-global) | `web/src/components/pages/project-settings.ts`, `web/src/components/pages/settings.ts` |
| Tests | `pkg/hub/resource_import_handler_test.go`, `pkg/config/*_test.go` |

---

## 8. Relationship to `hub-template-admin.md`

This doc implements the **Clone** and **Delete** actions from
`hub-template-admin.md` §2.4–2.5, scoped down to the shared resource list rather than
a new admin page, and reflecting that import (§2.6 there) is already shipped per
`resource-import-refactor.md`. It additionally **hardens delete/clone authz** (a gap
the parent doc assumed away) and confirms the `Locked` flag is currently latent.
Still deferred from the parent doc: the standalone admin page, filtering/sorting/
pagination, lock/unlock toggle (and a mechanism to *set* `Locked`), archive, bulk
actions, and usage indicators.
