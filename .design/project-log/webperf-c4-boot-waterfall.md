# webperf C4: boot waterfall, start in parallel and gate at the end (ptone/scion#5307)

Base: C3 head `d83bd75` (stacked on PR #5318, which is not upstream yet).
Part of the webperf epic (#5301). Only `web/src/client/main.ts` changes
(plus tests and the shared boot fixture).

## What changed

- **Boot requests overlap (a).** `init()` starts the onboarding check
  (`GET /api/v1/system/status`, now `redirectIfOnboardingIncomplete()`) at the
  top, next to the feature flags. Before, it waited for the auth fetch and the
  shell `whenDefined` calls first, and it was awaited by itself before the flags.
  Flags, onboarding check and the bounded timezone refresh are now awaited
  together before the first render. The redirect logic itself (skip paths,
  `sessionStorage`, `replaceState('/onboarding')`) is unchanged and still
  completes before `initialPath` is read.
- **Initial chunk preload (c).** `preloadInitialRoute()` runs as soon as the
  path is known, before `featureFlagsReady`. It starts the page import (plus
  the profile or chat shell import) for the initial path. It only fetches:
  `renderRoute` starts the same imports (the module loader dedupes them) and
  still checks flags and permissions first. A rejection is swallowed, and
  `renderRoute`'s own await reports the failure. The `/terminals` routes
  (including the legacy `/agents/<id>/terminal`) are skipped, so C3's async
  loader and `navigationId` guard are untouched. On an onboarding redirect the
  preloaded chunk is wasted. That happens on first-run only, and nothing is
  rendered from it.
- **Admin route overlap (b).** `renderRoute` now starts the imports through a
  shared `startRouteLoads(route, shellType)` *before* awaiting
  `loadAdminStatus(fresh)`. It runs the permission check and only then awaits
  the imports, so nothing renders before the check. While the admin-status
  request is in flight, the imports carry a no-op `catch`. A denied user is
  redirected without an unhandled rejection; a permitted user sees the failure
  from the later await.
- **Chat check moved first.** The native_chat flag check now runs before the
  admin check. The two route sets are disjoint. With chat disabled, a client
  navigation to `/chat` never starts the chat import. Only the boot preload (c)
  may fetch it, and it never renders.

## Numbers (dev-time vite build)

| | C3 | C4 | budget |
|---|---|---|---|
| main.js raw | 216,505 | 216,732 | 240,000 |
| main.js gzip | 58,358 | 58,417 | 65,000 |
| entry closure gzip | 135,519 | 135,578 | 140,000 |

## Tests

- `main-boot-waterfall.test.ts`: `boot issues system/status, flags and route
  chunk concurrently` holds system/status, settings/public, experiments and
  /auth/me. It asserts that all of them and the page-module import start while
  none has resolved, and that nothing renders until they are released.
  `admin route starts chunk import before admin-status resolves` holds the
  guard's fresh admin-status request and asserts that the page import has
  started and nothing has rendered.
- `main-chat-flag-gate.test.ts`: `chat route still blocked when native_chat
  disabled` boots at `/chat` with `nativeChatEnabled: false`. The chunk may be
  preloaded, but neither the page nor the chat shell is ever connected, and the
  URL ends at `/`.
- `__fixtures__/main-boot.ts` gains an optional per-path fetch override.
- The design asked for fake fetch and timers. The tests hold requests as
  unresolved promises that the test releases, without fake timers. The
  property is the order of starts and resolutions, and no timer is involved.
  The reviewer accepted this.

## Learnings

- Prove-it at dev time: against C3's `main.ts`, both waterfall tests fail and
  the chat regression test passes. Disabling the chat flag check makes the chat
  test fail.
- A `vi.mock` factory for a lazily imported page runs when `main.ts` first
  imports that module. Recording a timestamp in the factory is a cheap way to
  observe "the chunk import started" without a browser.
- The main-* route tests (C3's and these) are not part of any `tsc` project:
  the root tsconfig excludes `*.test.ts` and `__fixtures__`. Only vitest's
  esbuild transform sees them. Adding them to a test tsconfig would be a small,
  separate follow-up.

## Follow-up: onboarding redirect under a base path (upstream review on #3868)

- The onboarding redirect wrote a hardcoded `'/onboarding'` with
  `replaceState`. C4 moved the line into `redirectIfOnboardingIncomplete()`
  but did not introduce it: C3's `main.ts` has the same line. Under a non-root
  base path, the URL lost its base, so a reload left the app. It now uses
  `browserPath('/onboarding')`, like every other history write in `main.ts`.
- Regression test `main-onboarding-base-path.test.ts`: `keeps the base path
  when redirecting to onboarding` (stubs `BASE_URL` to `/scion/`). It fails
  without the fix (`'/onboarding'` instead of `'/scion/onboarding'`).
