# Directory Batch-Add for Injected Skills — Phase 1 Review Remediation

**Agent:** ps-dirbatch-p1-dev
**Date:** 2026-07-30
**Branch:** `scion/project-skills-dirbatch`
**Brief:** `/scion-volumes/scratchpad/briefs/ps-dirbatch-p1-fix.md`
**Phase 1 log:** `.design/project-log/ps-dirbatch-p1-dev.md`
**Commits:** `b9176cb4` (backend), `28c9ab74` (frontend)

Every finding from the code, security, and test reviews — blocking and
non-blocking — is addressed here. The four shared-layer items (archive size
bounds, orphaned cache dirs, rate limiting, cache-path race) and the backend
test-helper consolidation were explicitly out of scope and are untouched.

## Group 1 — blocking

**FIX-1 — cross-tenant credential use (C-1 / HIGH-1).** The handler validated
agent callers properly but for users only checked "is logged in". Supplying an
arbitrary `projectId` made `fetchRemoteForImport` mint that project's GitHub App
token, or read its `GITHUB_TOKEN` secret. User callers now go through the same
`authzService.CheckAccess(agent / project / create)` as `authorizeProjectImport`,
followed by a `GetProject` existence check so a typo'd UUID 404s instead of
silently degrading to an unauthenticated fetch.

The whole auth block moved into `authorizeSkillDiscover`, partly because the
handler was getting long and partly because the *reason* discovery needs
authorization at all is non-obvious — it persists nothing, but it spends
credentials. That rationale is now a doc comment on the helper rather than a
loose comment in the middle of the handler.

**FIX-2 — sourceUrl restricted to `https://github.com/` (HIGH-2/3, I-1).**
`config.IsRemoteURI` was the wrong predicate: it also accepts rclone connection
strings (`:local:/` would have the hub copy its own filesystem into a cache dir
and enumerate it) and bare `http://` URLs (an SSRF probe against hub-internal
hosts). Neither can ever yield a usable skill URI, so the check is now an
explicit `url.Parse` + scheme/host test. The comment names both rejected forms so
nobody "simplifies" it back to `IsRemoteURI`.

Note the host check is `strings.EqualFold` — hosts are case-insensitive, and
`HTTPS://GitHub.com/...` is a URL a user will realistically paste.

**FIX-3 — no raw error strings to the client (I-4 / MEDIUM-4).** This was the
sharpest finding. `fetchRemoteForImport` falls back to a sparse git checkout
whose remote is `https://x-access-token:<TOKEN>@github.com/...`, and
`sparseGitCheckout` returns git's stderr verbatim — so a failed private-repo
fetch could put a live GitHub token in an HTTP 400 body. Both error sites now log
via `s.resourceLog.Warn` and return a fixed message. The test asserts the token
value and the string `x-access-token` are absent from the response body.

**FIX-4 — `addEntries()` duplicate/409 defect (I-2).** My Phase 1 comment claimed
the project/user POST endpoints were "idempotent by design". They are not:
`handlers_skills_injection.go:189` returns 409 on `store.ErrAlreadyExists`. The
consequence was worse than an ugly error — the loop aborted on the first 409,
leaving a partial add, and re-clicking "Add Selected" 409'd again on the skills
that had just landed. Permanently stuck.

Two changes: pre-existing URIs are filtered against `this.rows` before anything
is sent, and the project/user loop collects per-item failures into one aggregate
error rather than throwing mid-batch. Hub scope needed the same filter for a
different reason — its PUT stores the list as given, so a re-discovery appended
duplicate rows with no 409 to warn anyone.

**FIX-5 — `?token=SECRET_NAME` suffix (I-3).** `discoverResourceDirs` builds child
URLs by plain concatenation, so `.../skills?token=X` became
`.../skills?token=X/child`, which `NormalizeSkillURI` rejects. Every child was
dropped and the user saw "no skills found" — a confusing failure for a feature
that looked like it worked. The suffix is split off before discovery and
re-attached per skill. It is also irrelevant to the fetch (which authenticates
from project credentials), so the fetch gets the bare URL; a test asserts the
secret *name* never travels to GitHub.

## Group 2 — non-blocking

- **FIX-6** — discovered directory names containing `?#&=` or `..` are skipped. A
  repo folder literally named `helper?token=PROD_SECRET` would otherwise smuggle
  a secret reference into a URI the UI invites the user to add.
- **FIX-7** — `http.MaxBytesReader` at 64 KiB. The body is two short strings.
- **FIX-8** — `Skipped []string` in the response, populated from the
  `discoverResourceDirs` return value that was previously discarded, so the UI
  can explain why a folder the user expected is missing.
- **FIX-9** — `closeDialog()` calls `resetDiscovery()`; the redundant call in
  `handleDiscoveryConfirm()` is gone.
- **FIX-10** — `showDiscoverButton` requires `https://github.com/`, matching the
  tightened backend contract.
- **FIX-11** — comment on why `handleDiscoverDirectory` posts the raw dialog URI
  rather than `dialogTransformed`.
- **FIX-12** — `TestSkillDiscoverKind` asserts `newStore == nil`, the invariant
  that makes the handler safe on a hub with no object storage.

## Group 3 — tests

**Backend** (`handlers_skills_discover_test.go`, 11 → 21 tests). New:
`_ProjectAuthToken` (captures the `Bearer` header on the wire — without it the
entire credential-scoping path was unverified), `_UserNotInProject` (403),
`_UnknownProject` (404), `_NonAdminUser` (200 — discover must not inherit the
admin-only rule from hub-scope *add*), `_FetchFailure`, `_NonRemoteURL`
(table-driven, 8 cases), `_MalformedBody`, `_TokenSuffix`,
`_SkipsUnnormalizableChild`, `_AllChildrenUnnormalizable`,
`_AgentOmitsProjectID`. Existing tests gained the outbound-URL assertion, the
`Skipped` assertion, and `code`-field assertions.

`mockSkillTarball` grew a `mockSkillTarballWithHook` variant so tests can inspect
the outbound request instead of the mock being URL-agnostic.

**Frontend** (`injected-skills-panel.test.ts`, 16 → 37 tests). New DOM-level
coverage for the discover button (present in URI mode, absent for `gh://`, absent
in search mode, and appearing only after the *real* `sl-input` handler runs
`normalizeSkillURIClient`), the selection dialog (Select All, indeterminate,
per-skill toggle asserting a new `Set` instance, Cancel with zero network calls),
`discoveryLoading` across both outcomes, and the FIX-4 cases (hub dedup, project
skip-already-present, 409 and 500 mid-batch continuation, partial-batch reporting
through `handleDiscoveryConfirm`).

## Notes on the fixes I did not make literally

**`_SkipsUnnormalizableChild` tests the reachable case, not the literal one.** The
brief asked for a child "whose derived URL can't be normalized" alongside valid
siblings. With FIX-6 in place that combination is not actually constructible: the
only per-child input to `NormalizeSkillURI` is the directory name, and every name
that would fail normalization (`..`, `?#&=`) is now rejected by the earlier guard.
The test therefore uses an unsafe-named sibling, which exercises the same
skip-and-continue path for the same reason, and the comment says so. The
`normErr` branch stays as defence in depth.

**`_FetchFailure` empties `PATH`.** A 404 from the mock transport sends
`fetchGitHubFolder` into its sparse-checkout fallback, which shells out to `git`
against the real github.com. `t.Setenv("PATH", "")` makes `exec.LookPath` fail
immediately, keeping the test hermetic and fast.

**Two extra tests beyond the brief.** `_UnknownProject` covers the `GetProject`
half of FIX-1 (the brief only specified the `CheckAccess` half), and
`_NonRemoteURL` asserts that no outbound request was attempted for any rejected
URL — the point of FIX-2 is that the fetch never happens, and a status-code-only
assertion would pass even if it did.

## Verification

- `go build ./...` — clean
- `go vet ./...` — clean
- `go test ./pkg/hub/` — pass (160s)
- `cd web && npm run typecheck` — clean
- `cd web && npx vitest run` — 103 tests across 6 files pass

Lint: `injected-skills-panel.ts` still carries its 35 pre-existing
prettier/unbound-method errors and no others — verified by diffing the normalized
eslint error set against the stashed baseline (net-new: none; removed: none).
