# Browser owner contract spike (P0.1)

Fixture only: no Hub, account login, live agent, WebSocket, or PTY. “Sessions” are
an in-memory set of fake agent IDs; execution counts prove selection deduplication.
Production code is unchanged. See [design](../../../.design/hosted/terminal-workspace.md).

## Run

From `web/`, after `npm ci`:

```sh
TERMINAL_OWNER_CHROMIUM=/usr/bin/chromium npm run test:e2e -- --config e2e/terminal-owner/playwright.config.ts
npm run typecheck -- --project e2e/terminal-owner/tsconfig.json
```

Omit `TERMINAL_OWNER_CHROMIUM` to use Playwright's installed Chromium. The local
config starts a loopback-only fixture server on port 4517. The fixture `*.pw.ts` files
deliberately do not match the root `*.spec.ts` suite, which starts a real Hub. CI must invoke
this config explicitly. No common harness changes or new packages are needed.
In Scion, remove inherited `SCION_*` variables from the test child environment.

For a real desktop Chrome run, start `node e2e/terminal-owner/server.mjs`, then
manually open `http://127.0.0.1:4517/` in Chrome. Use the same origin in every
participating window. Query parameters: `account=other`, `base=/other/`, and
`denyFocus=1` (a deterministic denial injection, **not** browser behavior).

## Proposed contract

| Field or behavior | Meaning                                                                                                                                                                                                                                        |
| ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `key`             | JSON tuple of actual page origin (scheme/host/port), normalized application base pathname without trailing slash, and account ID. The fixture uses query parameters for base/account; production must obtain trusted deployment/auth identity. |
| `generation`      | Random UUID generated only after acquiring the exclusive Web Lock. It identifies an owner lifetime, not a monotonically increasing epoch or authentication token.                                                                              |
| `requestId`       | Caller-generated ID for one selection; reuse for a retry, allocate a new ID for a new selection. Conflicting agent IDs for the same request ID are rejected.                                                                                   |
| Discovery         | Non-owner sends `discover(requestId)`; holder replies `owner(requestId, generation)`; caller sends `open(requestId, agentId, generation)`. Every envelope includes `key`.                                                                      |
| `ack`             | Matching request ID, agent ID and expected generation. `status: selected` means the owner selected the fake session; it never means browser activation succeeded.                                                                              |
| `focus`           | `document-focused`, `not-confirmed`, or `error`, sampled after `window.focus()`. Even `document-focused` is only document-level evidence. `desktopForeground` remains `unverified`.                                                            |
| Pending           | A 750 ms observation deadline returns `pending`. The original request stays live; retry uses the same ID. The deadline never releases, steals, or bypasses the lock.                                                                           |
| Unsupported       | Insecure context, absent Web Locks/BroadcastChannel, or rejected lock acquisition returns `unsupported`; no per-tab fallback or fake selection.                                                                                                |

Only a holder executes opens. `navigator.locks.request` uses `exclusive` and
`ifAvailable`; its callback stays pending until teardown. No heartbeat/lease,
`steal`, or queued automatic promotion exists. Browser lock authority, not a
late acknowledgment or owner advertisement, determines whether a later caller
may claim. An owner that shows another in-document view retains ownership.

The owner caches a promise per request before asynchronous focus work begins.
Duplicates across tabs share that result. Different request IDs for an existing
agent select again but reuse the fake session. Caches last only for the document
lifetime: this is **not** durable exactly-once execution across owner exit/reload.
Late messages addressed to an old generation do not execute in a replacement.
Explicit/pagehide teardown clears fake sessions before releasing authority.

Same-origin profiles/storage partitions are the coordination boundary. Different
origins cannot communicate through these APIs, even if they address the same Hub.
Different accounts/base paths use distinct lock and channel names. This cooperative
fixture protocol is not a security boundary against hostile same-origin scripts.
Account switching/logout transport teardown, bounded request-cache policy,
production message validation, and BFCache restore are later implementation work.

Sources checked 2026-09-19: [Web Locks](https://www.w3.org/TR/web-locks/),
[BroadcastChannel](https://html.spec.whatwg.org/multipage/web-messaging.html#broadcasting-to-other-browsing-contexts),
[window focus](https://html.spec.whatwg.org/multipage/interaction.html#dom-window-focus).
The lock callback controls ownership lifetime; focus is a request the browser can
decline. Browser messaging and ownership do not establish desktop activation.

Regression coverage also rejects acknowledgments with mismatched request ID,
agent ID, or generation while allowing the subsequent legitimate acknowledgment.
The loopback server returns HTTP 400 for malformed request targets; a raw HTTP
regression checks that a following healthy request still succeeds.

## Observed versus unverified

Runtime: Debian GNU/Linux 12, Linux 6.8.0-1064-gcp, system Chromium
152.0.7977.82, headless Playwright. No `DISPLAY`, `WAYLAND_DISPLAY`, desktop Chrome,
or window manager was available. Automated focus emulation is not desktop proof.

| Case                                                                            | Evidence                                                                                                                                                                                                                                                                                                                                                                           |
| ------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Simultaneous independent pages; repeated IDs                                    | Automated: one owner generation, one execution. Neither page has an opener.                                                                                                                                                                                                                                                                                                        |
| Different IDs selecting an existing session                                     | Automated: repeated selection, one session. Conflicting ID payload rejected.                                                                                                                                                                                                                                                                                                       |
| Denied-focus acknowledgment                                                     | Automated injected denial: selection succeeds, focus `not-confirmed`, foreground `unverified`, no second owner.                                                                                                                                                                                                                                                                    |
| Delayed owner                                                                   | Automated controlled processing pause: repeated deadlines remain pending, retry executes once after resume.                                                                                                                                                                                                                                                                        |
| Suspended JavaScript                                                            | CDP `Runtime.evaluate('debugger;')` with a confirmed `Debugger.paused` event; requester remains pending, one held lock, same-generation acknowledgment after resume. This is not browser lifecycle freezing.                                                                                                                                                                       |
| Owner close                                                                     | Automated: a later caller claims a new generation. Live state is not migrated.                                                                                                                                                                                                                                                                                                     |
| Hub origin/base path/account separation; missing APIs                           | Automated with real localhost versus 127.0.0.1 origins and injected missing capabilities.                                                                                                                                                                                                                                                                                          |
| Automatic Chrome lifecycle freeze                                               | **Unavailable/unverified.** `Page.setWebLifecycleState({state: 'frozen'})` returned but page stayed visible, no `freeze` event fired, and requests were processed. Disabling focus emulation and headless minimization also did not establish a freeze. Initial freeze test failed its pending expectation and was replaced with explicitly distinct debugger-suspension coverage. |
| Independent desktop tabs/windows, minimized owner, owner in Chat, opener closed | **Not run on a real desktop.** See manual procedure below.                                                                                                                                                                                                                                                                                                                         |

## Real desktop manual procedure

Acceptance clarification: debugger-confirmed suspension establishes the P0
unresponsive-owner contract. Actual lifecycle freezing remains unverified and is
carried to P3.3, not an additional P0 gate. Step 7 below is that later runtime
investigation. Desktop foreground evidence remains pending a real desktop run or
explicit user deferral. Debugger pause is not lifecycle freeze.

Record Chrome version from `chrome://version`, OS/window manager, fixture commit,
and whether the browser is normal/incognito. Use no automation, focus-enabling
flags, extension, or DevTools focus emulation. Record the physically visible
foreground window/tab independently of the JSON result.

1. Open fixture tab A, click **Open terminal**, then **Inspect local ownership**.
   Confirm `owner: true`; note its generation. Open B independently by typing the
   URL (no `window.open` relationship), change request ID to `desktop-b`, then
   click **Open terminal**. Record B's acknowledgment/focus fields and whether A
   actually foregrounded. Inspect A: one owner and one retained fake session.
2. Move A into a separate Chrome window. From B, use a fresh request ID and open
   again. Repeat with A minimized. Record each real foreground outcome separately;
   accepted selection with no foreground change must never create an owner in B.
3. In A, click **Show simulated Chat**. Repeat from B with a fresh request ID;
   inspect A for retained ownership and selection. This models an in-document mode
   change only, not the production SPA.
4. From a third fixture tab C, use the console once to open
   `window.open(location.href, '_blank')`. Have the opened page request a terminal,
   close C, then request from B. The singleton must remain reachable without C.
   Record whether any opener relationship changes actual activation behavior.
5. With the owner backgrounded/minimized, look for a real `not-confirmed` focus
   result. Record browser policy behavior without forcing a denial. Separately
   close every fixture tab and repeat A with `?denyFocus=1` to verify the injected
   fallback message; label it injected, never natural denial.
6. Close owner A, then use a new request ID in B. Inspect the new owner and different
   generation. Open a third page with `?account=other` and another with
   `?base=/other/`; each may claim independently.
7. For lifecycle freeze, register a `freeze` event observer before backgrounding
   the owner and attempt Chrome's supported discard/freeze diagnostics. Confirm an
   actual freeze before interpreting pending requests. If Chrome refuses to freeze
   a lock holder, record that fact rather than calling the case passed. Discard is
   document destruction, not freeze. Do not infer lifecycle freeze from debugger
   pause, a timeout, or a successful CDP command alone.

For every desktop row record: setup, request ID, owner generation, JSON result,
observed foreground tab/window, minimized state before/after, and pass/fail/not-run.
Best-effort focus is the intended contract; guaranteed activation is not promised.
