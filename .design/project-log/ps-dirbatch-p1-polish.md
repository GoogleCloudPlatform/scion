# Directory Batch-Add for Injected Skills — Phase 1 Final Polish

**Agent:** ps-dirbatch-p1-polish
**Date:** 2026-07-30
**Branch:** `scion/project-skills-dirbatch`
**Brief:** `/scion-volumes/scratchpad/briefs/ps-dirbatch-p1-polish.md`
**Prior logs:** `.design/project-log/ps-dirbatch-p1-dev.md`,
`.design/project-log/ps-dirbatch-p1-fix.md`
**Commits:** `dc070291` (backend), `ece9a0ae` (frontend)

All three pass-2 reviews returned APPROVE. This change closes the remaining
Low/Medium findings before the branch is signalled upstream. Nothing here is
architectural; every item is a localized correctness, diagnostic, or test gap.

The shared-layer items (archive size bounds, orphaned cache dirs, rate
limiting, cache-path race, redirect-hop validation) and the tarball-mock
consolidation remain out of scope and are untouched.

## Backend — `pkg/hub/handlers_skills_discover.go`

**P-1 — canonicalize `sourceUrl` after validation (N-1, N-2).** The URL was
validated and then handed downstream more or less as pasted, with only `?`
split off. It is now parsed once and canonicalized before anything else sees
it:

- *Host lowercased.* The gate is `strings.EqualFold` because hostnames are
  case-insensitive and `https://GitHub.com/...` is a URL people paste — but
  `config.DetectRemoteType` matches `"github.com"` exactly. So a mixed-case
  host passed the gate and then failed inside the fetch layer with a generic
  400: a valid URL producing a dead-end error.
- *Fragment stripped.* `discoverResourceDirs` builds child URLs by plain
  concatenation, so a surviving `#notes` yielded children like
  `.../skills#notes/alpha-skill` — syntactically plausible, unresolvable.
- *Query split off* into `tokenSuffix`, as before, but now via `u.RawQuery`
  rather than a string index.

**Beyond the brief: userinfo is also stripped.** Not in the brief, but it
falls out of the same canonicalization and is the one item here with a real
security edge. `https://x-access-token:SECRET@github.com/o/r/tree/main/skills`
parses with `Host == "github.com"`, so it passed the gate; `base` is then
written to the hub log on fetch failure *and* echoed back to the client in
`"no skills found at <base>"`. A pasted credential therefore reached both. It
is now cleared — discovery authenticates from project credentials, never from
the URL. Covered by `_StripsURLCredentials`, which asserts the credential
reaches neither the wire nor the response body.

**P-2 — require a `/tree/<ref>/` path (test Finding 1).** `api.NormalizeSkillURI`
requires that form. A bare-repo URL (`https://github.com/acme/repo`, or
`.../repo/skills`) passed the host check, spent a full tarball fetch, and then
failed normalization for *every* child — surfacing as "no skills found" for a
repo that plainly has skills. Now rejected up front with a message naming the
expected shape, and no outbound request.

This makes the `normErr` skip-and-continue branch unreachable from
well-formed input. It is kept as defence in depth, with a comment saying so —
the alternative (deleting it) would make an unaddressable directory fail the
whole discovery instead of just itself.

**P-3 — report silently-dropped directories (N-3).** The unsafe-name guard and
the `normErr` branch both `continue`d without recording anything, so a
legitimately-named folder that happens to contain `=` simply vanished from the
dialog. Both now append to a `droppedNames` slice that is merged into the
response's `Skipped` list, which already existed for marker-less siblings.
`_StandardSkillsDir` grows a `bad=name/SKILL.md` fixture and asserts it appears
in `Skipped` — the assertion is order-independent, since the two skip paths
contribute from different loops.

**P-4 — `ErrCodeDiscoverFailed`.** Added to `pkg/hub/errors.go` and used at all
three sites here, plus the three pre-existing literals in
`handlers_resource_import.go` so the constant is not immediately inconsistent
with its own package. Wire value unchanged.

## Frontend — `web/src/components/shared/injected-skills-panel.ts`

**P-5 — wire `skipped[]` into the dialog (test Finding 3).** The field was
added in FIX-8 specifically so the UI could explain missing directories, and
then never read — the rationale was stated and not honoured. `skipped[]` is now
captured into a `skippedSkillNames` state property, rendered as a muted note
below the selection list, and cleared in `resetDiscovery()` alongside the rest
of the discovery state (so it cannot survive a Cancel, same invariant the
existing lifecycle tests pin for `discoveredSkills`).

**P-6 (code side) — document the silent no-op.** `addEntries()` returns early
when every selected URI is already in `rows`. That is correct — an
all-duplicates batch has nothing to write, and issuing it would mean a no-op
PUT on hub scope — but the silence is easy to break by accident, so the reason
is now a comment rather than an inference.

The brief's N-4 (a toast for the all-already-present case) was explicitly
optional and is not done; the current behaviour is "both dialogs close", which
is defensible but does leave the user without confirmation. Worth revisiting if
it comes up in use.

## Tests

Backend, all in `handlers_skills_discover_test.go`:

- `_MixedCaseHost` — 200, and the outbound tarball URL is lowercase-host.
- `_SourceURLWithFragment` — 200, fragment absent from the fetch URL, children
  not corrupted.
- `_StripsURLCredentials` — pasted userinfo reaches neither GitHub nor the
  response body.
- `_SourceURLWithoutTreeRef` — four URL shapes, each 400 with an actionable
  message, and a `fetched` flag asserting no outbound request was made.
- `_CacheCleanup` — P-7. `HOME` is redirected to a temp dir so the assertion
  covers a private `~/.scion/cache/remote-templates`, and the entry count is
  compared before and after. Verified as a real canary: with the handler's
  `defer os.RemoveAll(cachePath)` removed, it fails with
  `cache entries ... = 1 after discovery, want 0`.
- `_StandardSkillsDir` extended for P-3.
- The two `discover_failed` literal assertions now use the constant.

Frontend, in `injected-skills-panel.test.ts`:

- Skipped note rendered with the right text (DOM assertion on `shadowRoot`,
  whitespace-collapsed).
- Pluralization (`1 folder ... was` / `2 folders ... were`) and absence of the
  note when the response carries no `skipped[]`.
- P-6: all-selected-URIs-already-present closes both dialogs with zero network
  calls and no error.
- `skippedSkillNames` reset added to the existing `openDialog()` /
  `closeDialog()` lifecycle tests.

## Verification

```
go build ./...                                    ok
go vet ./pkg/hub/                                 ok
gofmt -l pkg/hub/                                 clean
go test ./pkg/hub/                                ok (175s)
cd web && npm run typecheck                       ok
cd web && npx vitest run .../injected-skills-panel.test.ts
                                                  40 passed (was 37)
```

Backend discover tests: 26 → 31 top-level cases. ESLint on the two changed
frontend files is back at its pre-change baseline (62 problems, all
pre-existing repo-wide prettier/return-type noise); the repo does not lint
clean today, so the baseline comparison is the meaningful check.
