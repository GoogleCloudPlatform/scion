# Phase 2b Fix Log: Hub gh:// Resolver Defects

**Date:** 2026-07-29
**Agent:** ps-cache-p2b-fix
**Branch:** `scion/ps-cache-p2b-fix`
**Base:** `main` @ 13db010 (Phase 1: broker-singleton resolution cache, #885)

## Summary

Code review blocked Phase 3 ("route gh:// through the Hub") on three defects in
the Hub-side gh:// resolution path added in Phase 2. All three are fixed here.
While fixing the first I found a fourth, related defect that the brief did not
mention and that would have kept the feature broken; it is covered below and
changed the shape of the fix.

The `gh://` scheme is **still routed to the local resolver** on this branch.
Nothing here flips the switch — that remains Phase 3's job. These changes only
make the Hub path correct so that flip can happen.

## The four defects

### 1. Per-file hash format mismatch (blocking)

`ghListContents` stores GitHub's git blob object ID (bare 40-char SHA-1, from
the Contents API `sha` field) as `GitHubFileEntry.Hash`. The broker's
`installOneSkill` compared that against `transfer.HashFile`, which returns
`"sha256:<hex64>"`. The two can never be equal, so every Hub-resolved gh://
install failed verification.

### 2. Bundle hash mismatch (blocking — not in the brief)

The same mismatch exists one level up, and it matters because it invalidates
the obvious fix for defect 1.

The brief proposed sending an empty per-file hash and skipping the per-file
check, on the reasoning that "the bundle hash still protects integrity". It
does not: the Hub's `computeBundleHash` digests git blob IDs, while the
broker's bundle check digests `sha256:` values. `skill.Hash` is non-empty, so
the broker computes a bundle hash over different inputs and fails there
instead. Installs would still have been broken 100% of the time, just with a
different error message — and with per-file verification silently dropped.

So the empty-hash approach was not viable on its own. Two ways out:

- **Have the Hub download file bytes** so it can publish sha256 digests.
  Uniform hash format everywhere, no broker change. But it puts N file
  downloads on the Hub for every cache miss, which is most of what moving
  resolution to the Hub was meant to avoid.
- **Have the broker verify against the git blob ID.** The Hub stays at two API
  calls per miss, and verification is preserved rather than discarded.

Took the second. The two formats are distinguishable on sight — `sha256:`
prefix versus bare 40-hex — so the broker hashes with whichever algorithm the
expected value is written in (`hashFileAs` / `hashBytesAs`). Recording the
result in the published format keeps the recomputed bundle hash comparable to
the Hub's, so both checks pass and both remain real checks.

On SHA-1: the git blob ID is not a general-purpose integrity primitive, and the
new `pkg/transfer` helpers say so. It is adequate *here* because the entire
trust chain for this path is already git's — content is fetched over TLS from
raw.githubusercontent.com at a pinned commit SHA, itself a SHA-1 git object ID.
Layering sha256 on top would not strengthen anything the commit pin does not
already fix, and would cost the Hub a full content download per miss.

The brief's `f.Hash != ""` guard is still in place, but now means "this
resolver published no digest" rather than being the mechanism that makes gh://
work.

### 3. Expiring download URLs for private repos (blocking)

For private repos the Contents API `download_url` is a CDN link carrying a
short-lived signed token. The Hub caches entries for 30 minutes; the token dies
in minutes. Any cache hit past the token's lifetime yielded a 404.

`ghListContents` now builds the URL from the resolved commit SHA, which is
immutable and therefore permanent. The origin is configurable
(`GitHubAppServerConfig.RawBaseURL`, default `https://raw.githubusercontent.com`)
for GitHub Enterprise and for tests.

That leaves authentication. **The Hub does not send a credential**, contrary to
one option floated in the brief (adding `GitHubToken` to `DownloadURLInfo`). A
Hub-minted GitHub App token in an API response body would be persisted to the
broker's on-disk resolution cache and exposed in any request logging — a
meaningful widening of that token's blast radius for a convenience gain.

Instead the broker supplies its own `GITHUB_TOKEN` (already present as
`defaultGHToken`, already used by the local resolver) via
`agent.ContextWithGitHubToken`. `downloadSkillFile` presents it **only to
GitHub hosts**, so a Hub response naming an arbitrary origin cannot siphon the
broker's credential. Context injection was chosen over a new `ResolvedFile`
field because the token is a broker-environment property, not a per-file one,
and a `ResolvedFile` field would need `json:"-"` to stay out of the disk cache
anyway — which would make it useless on the restart-within-TTL path.

**Known limitation:** this covers private repos reachable by the broker's own
`GITHUB_TOKEN`. A repo readable *only* via the Hub's GitHub App installation
will still 404 at download time. That is a narrower gap than today's behaviour
(where every private-repo cache hit 404s), but it is a real gap and should be
tracked separately — the fix is for the Hub to proxy the content, not to ship
its token to brokers.

### 4. Empty ref not defaulted to HEAD

`resolveGitHubSkill` passed an omitted ref through verbatim, unlike the local
resolver (`github_skill_resolver.go` `resolveCommitSHA`), which defaults to
`HEAD`. Two consequences: unpinned URIs hit an unexpected code path in
`ghResolveCommitSHA`, and — because the ref is part of the cache key —
`gh://o/r/p` and `gh://o/r/p@HEAD` kept separate entries for the same commit.
Defaulting happens before the key is computed, so both spellings now share one
entry.

## Files changed

**`pkg/transfer/hash.go`** — added `GitBlobHashBytes`, `GitBlobHashFile`,
`IsGitBlobHash`. Git object IDs are SHA-1 by definition; the import and both
call sites carry `//nolint:gosec` with the rationale.

**`pkg/agent/skill_resolver.go`**
- `hashFileAs` / `hashBytesAs`: hash using the algorithm the expected value is
  written in.
- `installOneSkill`: use `hashFileAs`; skip comparison on an empty expected
  hash; pass the GitHub token to `downloadSkillFile`.
- `verifyInstalledSkillHash` (the cache-hit path): same dual-format handling,
  which it needed too — it hardcoded sha256.
- `ContextWithGitHubToken` / `GitHubTokenFromContext`.
- `downloadSkillFile`: new `ghToken` parameter; `isGitHubHost` gate.

**`pkg/hub/github_resolution_store.go`** — `ghListContents` takes `rawBase` and
builds permanent URLs via the new `ghRawContentURL` / `ghEscapePathSegments`.

**`pkg/hub/skill_handlers.go`** — default ref to `HEAD` before computing the
cache key; thread `rawBase`; documented the hash format on
`buildResolvedSkillResponse`.

**`pkg/hub/server.go`** — `GitHubAppServerConfig.RawBaseURL`.

**`pkg/runtimebroker/handlers.go`** — inject `defaultGHToken` into the
provisioning context.

## Tests

New coverage:

- `pkg/transfer`: git blob hashing checked against fixtures generated by real
  `git hash-object` (empty, `hello\n`, `hello world`); `IsGitBlobHash` format
  discrimination including a sha256 digest, wrong lengths, uppercase, non-hex.
- `pkg/agent`: install succeeds end-to-end with git-blob per-file hashes *and*
  a bundle hash over them (the defect-1+2 regression test); tampered content
  under a git-blob hash is still rejected; empty hash installs; `hashFileAs`
  algorithm selection; `isGitHubHost` including look-alike hosts
  (`github.com.evil.test`, `notgithub.com`, `evilgithubusercontent.com`); token
  is not sent to a non-GitHub host; token context round-trip.
- `pkg/hub`: `ghRawContentURL` incl. trailing-slash and percent-escaping;
  `ghListContents` produces token-free permanent URLs and skips directories;
  `rawBase` override; a test documenting that the empty and `HEAD` cache keys
  differ, which is *why* defect 4 mattered.

Updated the four existing `downloadSkillFile` call sites for the new parameter.

Results — all green, no pre-existing failures observed:

```
go build ./pkg/... ./cmd/...                     ok
go vet ./pkg/agent/... ./pkg/transfer/... ./pkg/runtimebroker/...   ok
go vet ./pkg/hub/...                             ok
go test ./pkg/transfer/                          ok   0.0s
go test ./pkg/agent/                             ok   7.2s
go test ./pkg/hub/                               ok 189.9s
```

**Environment note:** the shared host volume repeatedly hit 100% during this
work (`no space left on device` from the Go toolchain, unrelated to these
changes). Commands were re-run once space freed; the results above are from
clean runs. Worth flagging to infra — it is not specific to this branch.

## Follow-ups

1. **Private repos behind the Hub's App installation** still fail at download
   (see defect 3). Proper fix: proxy content through the Hub rather than
   distribute its token.
2. **`ResolvedFile.Content` is `json:"-"`**, so the restart-within-TTL path
   falls back to `downloadSkillFile`. With the token now threaded through, that
   path works for broker-token-readable repos — previously noted as a
   limitation in the field docs, which are now partly stale and could be
   revisited alongside follow-up 1.
3. **Phase 3 can proceed**: routing `gh://` to the Hub resolver is a one-line
   change in `pkg/runtimebroker/handlers.go`, deliberately not made here.

---

## Addendum: cross-project authorization gap in `handleSkillsResolve`

**Date:** 2026-07-29
**Agent:** ps-cache-p2b-authz
**Commit:** one commit on top of the four fixes above.

A fifth defect, found in review of this branch. Unlike the other four it is a
security defect rather than a correctness one, so it is worth calling out
separately.

### The gap

In `handleSkillsResolve`, the `gh://` branch ran at the top of the per-skill
loop, *before* any identity or access check:

```go
for _, skillRef := range req.Skills {
    if strings.HasPrefix(skillRef.URI, "gh://") {
        ghResolved, err := s.resolveGitHubSkill(ctx, skillRef.URI, req.ProjectID)
        ...
        continue
    }
    // registry path — access-checked further down
```

`resolveGitHubSkill` calls `resolveGitHubToken(ctx, req.ProjectID)`, which
loads that project and mints its GitHub App token. `req.ProjectID` is
caller-supplied and was never validated against the caller's identity, so any
authenticated principal could name *any* project and have the Hub mint that
project's installation token — then use the resulting resolution to read files
from repositories that installation can reach. The registry path immediately
below has always checked access; the `gh://` path simply bypassed it by
`continue`-ing first.

This was latent rather than exploitable in production today, because Phase 2
left `gh://` routed to the local resolver and nothing calls the Hub endpoint
with a `gh://` URI yet. Phase 3 flips exactly that switch, so it had to be
closed before the flip, not after.

### The fix

Authorize the caller against `req.ProjectID` before any `gh://` resolution,
mirroring the check already used for project-scoped skill creation:

- **Agent identity** — `agentIdent.ProjectID() == req.ProjectID`, the same
  confinement agents get everywhere else.
- **User identity** — `CheckAccess(userIdent, Resource{Type: "skill",
  ParentType: "project", ParentID: req.ProjectID}, ActionRead)`. `ActionRead`
  rather than `ActionCreate`: resolving reads a skill, it does not publish one.
- **No identity** — denied.

A failed check appends `ResolveSkillError{Code: "forbidden"}` and skips the
entry, matching how the registry path reports a denial. The endpoint stays
`200` with per-URI errors; one forbidden reference does not fail the batch.

Two deliberate scoping choices:

- **Only when `req.ProjectID != ""`.** An empty project ID makes
  `resolveGitHubToken` return the `"public"` scope with no token, so there is
  no credential to borrow and nothing to authorize. Checking anyway would break
  anonymous resolution of public repos for no security gain.
- **Evaluated once per request, not per URI**, and only when the batch actually
  contains a `gh://` reference (`hasGitHubSkillRef`). The decision cannot vary
  between URIs in a batch — same caller, same project — so a 50-item batch
  should not cost 50 identical `CheckAccess` calls, and a batch with no `gh://`
  references should cost none.

### Files changed

**`pkg/hub/skill_handlers.go`** — `hasGitHubSkillRef` and
`Server.canUseProjectGitHubToken` helpers; the guard in `handleSkillsResolve`.

### Test

`TestSkillAuthz_Resolve_GH_ForbiddenProject` in
`pkg/hub/skill_handlers_authz_test.go`, alongside the existing
`TestSkillAuthz_Resolve_ForbiddenSkill`. Bob (not a member of Alice's project)
POSTs a `gh://` URI with Alice's `ProjectID` and must get
`Code: "forbidden"`.

The test needs no GitHub mock, and that is the point: the assertion is
specifically `forbidden` and not `resolve_failed`. Reverting the guard turns it
into `resolve_failed` — the request reaches the GitHub API before failing — so
the test distinguishes "denied" from "attempted and happened to fail", and
would not pass on a fix that merely made the GitHub call fail. Verified by
temporarily disabling the check and confirming that exact transition.

```
go test ./pkg/hub/... -run TestSkillAuthz    ok  1.4s
gofmt -l                                     clean
```
