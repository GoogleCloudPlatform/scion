# webperf C7: shared-data dedup, client-only parts (ptone/scion#5311)

Base: C4 head `3fa70f6` (stacked on PR #5321, which is stacked on #5318; neither
is upstream yet). Part of the webperf epic (#5301). Client only; no hub change.

## What changed

- **Public settings read once per page load.** `client/server-feature-flags.ts`
  exports `getPublicSettings()` (memoized, single-flight) and
  `invalidatePublicSettings()`. Boot (`applyServerFeatureFlags`) reads through
  it, so the boot request fills the cache that `agent-create.ts`,
  `agent-configure.ts`, the `project-settings.ts` telemetry fallback and the
  terminal capture-scope dialog (`terminal-pane.ts`) then reuse. Before, each
  of those sent its own `GET /api/v1/settings/public` every time it opened.
  - Failures (network error, non-OK status, non-JSON or non-object body) are
    not cached; the next caller retries.
  - Invalidation: after a successful `PUT /api/v1/admin/server-config` in
    `admin-server-config.ts` (the only page that edits the fields the endpoint
    returns: chat toggle, telemetry and auto-expose defaults, hub agent
    defaults, `agent_secrets.user_scope_only`), and in `terminal-pane.ts` after
    a capture is rejected with `secret_scope_restricted`, so the "retry after a
    rejection" flow still reads a since-changed policy. A generation counter
    stops a request that was in flight at invalidation time from refilling the
    cache with the old value.
  - The response is hub-global (no per-user fields), and signing out or
    switching hubs is a full page load, so the cache cannot serve one user's or
    one hub's values to another.
  - Trade-off: an admin edit made in another tab or browser now reaches an
    open tab on its next page load instead of on the next dialog open. The
    server still enforces the policy; the terminal case self-corrects through
    the rejection invalidation.
- **`/auth/me` at boot.** Already a single request at the base when SSR
  provides the user (`loadPreferredTimeZone`; `fetchCurrentUser` only runs
  without an SSR user). The finding's "twice" no longer holds. Added the
  acceptance test as a regression guard; no code change.
- **Projects page seeds hub capabilities.** `projects.ts` calls the existing
  `seedHubProjectCapabilities()` with its list's `_capabilities` (as `home.ts`
  already did), so the next page's create-project check
  (`fetchHubProjectCapabilities`) sends no `projects?limit=1`. The list's
  capabilities are hub-scope (`ComputeScopeCapabilities(..., "", "", "project")`),
  independent of the `scope=` filter.
- **Profile nav GitHub App check.** `profile-nav.ts` read every one of the
  user's projects (`/api/v1/projects?mine=true`, hub default limit 500) on
  every profile page, in parallel with `/api/v1/github-app`, to test
  `.some(githubInstallationId)`. It now asks `/api/v1/github-app` first and
  reads projects only when the app is configured with an installation URL,
  in pages of 100 that stop at the first match, at most 5 pages. That is the
  same 500-project window the old request saw, so the link shows in exactly
  the same cases.

## Tests

- `public settings fetched once across dialogs`
  (`components/pages/public-settings-shared.test.ts`): boot, then create form,
  configure page and project settings fallback; one request in total.
- `boot issues one /auth/me when SSR user present` (`client/main-auth-me.test.ts`).
- `projects page seeds capabilities (no projects?limit=1 on next nav)`
  (`components/pages/projects-capabilities.test.ts`).
- Cache unit tests in `server-feature-flags.test.ts` (sharing, failures not
  cached incl. a synchronously throwing fetch, invalidation during flight),
  admin save invalidation in `admin-server-config.test.ts`, terminal reopen and
  rejection refetch in `terminal-pane.test.ts`, and the GitHub App link cases
  in `profile-nav.test.ts`.

## Learned

- A module-level cache leaks between tests in one vitest file (each file has
  its own module registry, tests inside it share it). Files that serve
  different settings per test reset it in `beforeEach`/`afterEach`.
- In an `async` IIFE, a synchronous throw before the first `await` runs
  `catch`/`finally` synchronously, before the caller stores the promise. Clear
  the in-flight slot from a `.then` on the stored promise, not from `finally`.
- The terminal pane's request-sequence guard still matters: overlapping
  requests now only happen when the cache is invalidated between two opens,
  and its two tests do that explicitly.
