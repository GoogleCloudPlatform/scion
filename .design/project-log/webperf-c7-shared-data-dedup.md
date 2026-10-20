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
    not cached; the next caller retries. The cached object is frozen, since
    every caller shares it.
  - Invalidation: in `admin-server-config.ts` after a successful
    `PUT /api/v1/admin/server-config` (Save & Reload) and after a successful
    section reset (`DELETE /api/v1/admin/server-config/sections/{name}`). Those
    are the two admin writes that can change the fields the endpoint returns
    (chat toggle, telemetry and auto-expose defaults, hub agent defaults,
    `agent_secrets.user_scope_only`); the federation page's PUT sends only
    federation keys and needs none. A generation counter stops a request that
    was in flight at invalidation time from refilling the cache with the old
    value.
  - The terminal capture-scope dialog keeps the fresh read of design
    ptone/scion#2291 §7: it drops the cache before each open, so a policy an
    admin changed in another tab (on or off) applies on the next open, and its
    fresh result refreshes the copy the other pages read. The first review
    round caught that a cached read here lost the "turned off elsewhere" case.
  - The response is hub-global (no per-user fields), and signing out or
    switching hubs is a full page load, so the cache cannot serve one user's or
    one hub's values to another.
  - Trade-off: for the create, configure and project settings pages, an admin
    edit made in another tab now reaches an open tab on its next page load (or
    after the terminal dialog's fresh read), instead of on the next page open.
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
  the same cases. An empty page also ends the scan (upstream review), as a
  guard: the hub sets `nextCursor` only when more rows follow.

## Tests

- `public settings fetched once across dialogs`
  (`components/pages/public-settings-shared.test.ts`): boot, then create form,
  configure page and project settings fallback; one request in total.
- `boot issues one /auth/me when SSR user present` (`client/main-auth-me.test.ts`).
- `projects page seeds capabilities (no projects?limit=1 on next nav)`
  (`components/pages/projects-capabilities.test.ts`).
- Cache unit tests in `server-feature-flags.test.ts` (sharing, failures not
  cached incl. a synchronously throwing fetch, invalidation during flight,
  frozen result), admin save and section reset invalidation (success and
  failure) in `admin-server-config.test.ts`, the terminal dialog's fresh read
  past a warm cache in `terminal-pane.test.ts`, and the GitHub App link cases
  in `profile-nav.test.ts`.

## Learned

- A module-level cache leaks between tests in one vitest file (each file has
  its own module registry, tests inside it share it). Files that serve
  different settings per test reset it in `beforeEach`/`afterEach`.
- In an `async` IIFE, a synchronous throw before the first `await` runs
  `catch`/`finally` synchronously, before the caller stores the promise. Clear
  the in-flight slot from a `.then` on the stored promise, not from `finally`.
- The terminal pane's request-sequence guard still matters: each open drops
  the cache and starts its own request, and the generation counter keeps an
  older open's response out of the cache.
