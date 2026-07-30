# Directory Batch-Add for Injected Skills — Phase 1

**Agent:** ps-dirbatch-p1-dev
**Date:** 2026-07-30
**Branch:** `scion/project-skills-dirbatch`
**Design:** `/scion-volumes/scratchpad/projects/project-skills/design-dirbatch-skills.md`
**Commits:** `276f0914` (backend), `f05fbd56` (frontend)

## What was built

Phase 1 of directory batch-add: point the injected-skills panel at a GitHub
directory of skills, pick a subset from a checkbox list, and add them all in one
operation. Works across all three scopes (project, user, hub). Phase 2 (the
`--from-directory` CLI flag) is not in scope here.

## Commit 1 — backend discover endpoint

`POST /api/v1/skills/discover-directory` fetches a remote GitHub directory,
scans it for subdirectories containing a `SKILL.md`, and returns
`{ skills: [{uri, name}], count }`. Nothing is persisted — this is a read-only
probe.

**Files:**
- `pkg/hub/handlers_skills_discover.go` (new) — `skillDiscoverKind`,
  `DiscoverSkillsRequest` / `DiscoveredSkill` / `DiscoverSkillsResponse`,
  `handleSkillsDiscoverDirectory`.
- `pkg/hub/handlers_skills_discover_test.go` (new) — 11 tests.
- `pkg/hub/server.go` — one route registration line next to
  `/api/v1/resources/discover`.

**Reuse:** `fetchRemoteForImport` (tarball fetch + GitHub App token /
`GITHUB_TOKEN` secret auth) and `discoverResourceDirs` (leaf-vs-parent layout)
are called verbatim; `api.NormalizeSkillURI` produces each canonical URI. Only
`skillDiscoverKind` and the handler are net-new.

## Commit 2 — frontend discovery dialog and batch-add

**File:** `web/src/components/shared/injected-skills-panel.ts`
(+ `injected-skills-panel.test.ts`, new, 16 tests)

Five new `@state()` props, plus `showDiscoverButton` (getter),
`handleDiscoverDirectory()`, `renderDiscoveryDialog()`, `addEntries()`,
`handleDiscoveryConfirm()`, and `resetDiscovery()`. The selection dialog is
rendered from `render()` alongside `renderDialog()`.

## Key decisions

**Where `skillDiscoverKind` lives.** The brief allowed `resource_import.go` "or a
small adjacent file". It went in `handlers_skills_discover.go` so the kind sits
next to its only consumer; `resource_import.go` holds the two kinds that have a
`newStore`, and this one deliberately does not.

**`newStore: nil` is safe.** Only `discoverResourceDirs` is ever called with this
kind, and that function never touches `newStore`. Documented in a comment so a
future caller does not assume the field is populated.

**No `GetStorage()` guard.** Template and harness-config discovery bail out when
hub object storage is unconfigured because their discover step is a prelude to
an import that writes there. Skill discovery never writes, so the guard would
only produce spurious 503s on hubs with no storage backend.

**Empty result is a 400, not an empty 200.** Matches `discoverFromRemote`'s
`"no scion <noun> found at <url>"` behaviour and gives the UI something
actionable to display. The frontend also treats a `skills: []` 200 as an error,
so a future relaxation of the backend contract cannot produce an empty dialog.

**Agent auth also *narrows* the fetch.** Beyond the design's 403 checks (missing
`ScopeAgentCreate`, or a `projectId` belonging to someone else), the handler
overwrites `req.ProjectID` with the agent's own project when the caller omitted
it. Without this an agent that left `projectId` off would silently get an
unauthenticated fetch and fail on private repos.

**Discover-button gating reads `dialogTransformed` first.** This is the
substance of the Q3 B+C+D blend: a GitHub URL under a standard `skills/` path
normalizes to `gh://` and so offers no discovery, while the same repo's
`skills` parent stays `https://` and does. Falling back to the raw `dialogUri`
only matters before normalization has produced a transform.

**`addEntries()` vs. N × `addEntry()`.** Hub scope's PUT-whole-list API makes the
naive loop O(N) round-trips with N-1 pointless intermediate server states.
`addEntries()` builds the list once and PUTs once. Project/user scope keeps the
per-item POST path unchanged — those endpoints are idempotent, and duplicating
their logic would be the more fragile choice.

> **Correction (see `ps-dirbatch-p1-fix.md`, FIX-4):** the project/user POST
> endpoints are *not* idempotent — they return 409 on a duplicate skill URI. The
> claim above was wrong and the resulting partial-add defect was fixed in
> commit `28c9ab74`.

**Dialog duplicated, not extracted.** Per Alt A2 in the design: the two checkbox
pickers operate on `string[]` vs `{uri, name}[]`, so sharing would need generics
or a render slot for ~70 lines of markup. The `.selection-*` CSS was copied into
the panel's styles for the same reason (`resourceStyles` does not carry it).

## Verification

- `go build ./...` — clean
- `go vet ./...` — clean
- `go test ./pkg/hub/...` — all pass (166s)
- `cd web && npm run typecheck` — clean
- `cd web && npx vitest run` — 82 tests across 6 files pass

Lint: `injected-skills-panel.ts` carries 35 pre-existing prettier/unbound-method
errors on `main`. The changed file is at parity — verified by diffing the eslint
error set against the stashed baseline. The new `.test.ts` file reports the same
single "not in tsconfig" error every existing `*.test.ts` in the repo reports.

## Acceptance criteria

All Phase 1 backend criteria are covered by tests, including the three auth
cases and the leaf-URL case. Frontend criteria are covered by unit tests for the
button gating, the one-PUT hub batch, the N-POST project batch, and inline error
display; the visual criteria (loading state, indeterminate Select All, count
label) are implemented but verified by reading rather than by test.

## Note for the EM

The brief said branch `scion/project-skills-dirbatch` was already created from
`origin/main`; it did not exist on the remote. I created it locally from
`origin/main` (which `HEAD` already matched, at `d693a5ee`). Not pushed —
pushing is the EM's step.
