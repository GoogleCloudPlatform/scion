# Terminal workspace P0.1 browser ownership spike

Date: 2026-09-19. Base: `1bb405704a96c2a03f2a1b2b95719edc5d0cda94`.
Fixture implementation: `4ade259`.

Added an isolated executable fixture under `web/e2e/terminal-owner/`, with its own
Playwright configuration and loopback server. No production module, shared harness,
Hub, broker, live agent, extension, or dependency change. The
[fixture README](../../web/e2e/terminal-owner/README.md) records the proposed message
contract, observation matrix, and exact real-desktop manual procedure.

Exclusive Web Lock ownership gates fake session selection. BroadcastChannel
discovery/open/ack envelopes include the hub-origin/base-path/account key, request
ID, and owner generation. An owner-local promise cache deduplicates concurrent
retries. Timeouts return pending and never steal authority. Selection acknowledgment
is separate from document focus observation; desktop foreground remains unverified.
Request/session state lasts only for the owner document lifetime.

## Verification

Every test/build command used a child environment with inherited `SCION_*` keys
removed. Runtime: Node 20.20.2, npm 10.8.2, Chromium 152.0.7977.82 on Debian 12,
Linux 6.8.0-1064-gcp. Browser tests were headless, not real desktop Chrome.

| Command (from `web/` unless stated)                                                                                                   | Result                                                                                                                                                     |
| ------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TERMINAL_OWNER_CHROMIUM=/usr/bin/chromium npm run test:e2e -- --config e2e/terminal-owner/playwright.config.ts`                      | 11 passed, 12.5 seconds                                                                                                                                    |
| `npm run typecheck -- --project e2e/terminal-owner/tsconfig.json`                                                                     | Pass; checks the otherwise-excluded fixture test/config TypeScript                                                                                         |
| `node --check e2e/terminal-owner/coordinator.js` and `node --check e2e/terminal-owner/server.mjs`                                     | Pass                                                                                                                                                       |
| Fixture-only ESLint, `--no-eslintrc`, browser/node + es2022 environments, ES2022 modules, `no-undef:error` and `no-unused-vars:error` | Pass for both JS files                                                                                                                                     |
| `npm run typecheck`                                                                                                                   | Pass                                                                                                                                                       |
| `npm run build`                                                                                                                       | Pass                                                                                                                                                       |
| `npm run test`                                                                                                                        | Initial default-concurrency run: 56 files passed, one setup-hook timeout in unchanged `role-binding-assignment-form.test.ts:804`; 1150 passed, one skipped |
| `npm run test -- --maxWorkers=2`                                                                                                      | Pass: 57 files, all 1151 tests, zero skipped                                                                                                               |
| `npm run lint`                                                                                                                        | Existing baseline failure: 806 errors, 2105 warnings in unchanged `src`; root lint does not inspect the new fixture directory                              |
| `make ci` (repository root)                                                                                                           | Pass: formatting, vet, Go tests, build                                                                                                                     |
| `git diff --check`                                                                                                                    | Pass                                                                                                                                                       |

Baseline evidence: `git diff 1bb405704a96c2a03f2a1b2b95719edc5d0cda94 -- web/src
web/package.json web/package-lock.json web/tsconfig.json web/vitest.config.ts
web/.eslintrc.cjs` is empty. The isolated fixture is excluded from the Vitest `src`
include and root lint command. No source/config changes were made to suppress lint
or change the unit tests; lowering worker concurrency resolved the setup timeout.

The first election test failed before the coordinator existed, then passed after
implementation. Final coverage includes independent simultaneous tabs and repeated
request IDs, conflicting ID payloads, session reuse, injected denied focus,
delayed-owner retries, confirmed debugger suspension, owner close/reclaim,
origin/base/account isolation, unsupported APIs/context, and stale-generation opens.

## Limits and findings for the next phase

`Page.setWebLifecycleState({state: 'frozen'})` did not establish an actual freeze:
the owner stayed visible, no `freeze` event arrived, and requests were acknowledged.
Disabling focus emulation and headless minimization did not change that result.
The initial test expecting pending therefore failed. Automatic browser lifecycle
freezing is explicitly **unverified**, not passed by weakening that assertion.

The replacement test proves a different condition: an actual JavaScript suspension,
with a `Debugger.paused` event from `Runtime.evaluate('debugger;')`. While suspended,
the caller receives pending, remains non-owner, and observes one held lock. After
resume, the same generation acknowledges safely. `Debugger.pause` alone schedules
a pause at the next task and can leave an idle document waiting; the explicit
evaluation is necessary to make this fixture deterministic.

No desktop display/window manager or desktop Chrome was available. Independent
real windows, minimized windows, natural denied focus, owner-in-Chat foregrounding,
and opener-closure foreground behavior remain **not run**. This blocker was reported
to the manager immediately; manual steps are ready. Injected denial demonstrates
the fallback acknowledgment contract only. This candidate does not close those
desktop acceptance gates and makes no guarantee of foreground activation.

Keep future production concerns explicit: authenticated account/deployment keys,
message validation, logout teardown, bounded request-cache policy, BFCache restore,
and real transport closure before lock release. These are not implemented in this
P0 fixture. The actual desktop focus and lifecycle-freeze observations are still
needed; a timeout must never be used as proof that ownership is vacant.
