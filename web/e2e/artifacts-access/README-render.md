# Artifact page view and render journey (R2)

A Playwright journey of the artifact page's view and render, graded at the UI. It lives in the same suite directory as
the access journey (`README.md`) and shares its global setup, state file and output dir. It has its own config,
`playwright.render.config.ts`, which has no HTML reporter (see Grading), and it refuses any other config. See
the header of `artifacts-render.pw.ts` for the rows.

## Run

Local throwaway hub (default). Builds the binary and web assets unless `E2E_PREBUILT=1`:

```bash
cd web
npx playwright test -c e2e/artifacts-access/playwright.render.config.ts
```

No runtime broker, container runtime, credentials or external network is needed. The journey turns on `hub.artifacts`
through the admin experiments API only if it is off, and resets the override in the cleanup only if it set it.

Three member test identities each act in their own browser context:

| Key        | Role                                                                     |
| ---------- | ------------------------------------------------------------------------ |
| `owner`    | publishes both artifacts and all three bundle versions; owns the project |
| `member`   | the reader: a member of the project, no grant                            |
| `outsider` | in no project                                                            |

Existing non-IAP hub: set `E2E_TARGET_URL`, and for each key `E2E_R2_<KEY>_CODE_FILE` and `E2E_R2_<KEY>_EMAIL`
(OWNER, MEMBER, OUTSIDER). The experiment toggle works as in the access journey (`E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1`
with `E2E_ADMIN_TOKEN_FILE`).

## Fixtures

`render-fixtures.ts` writes them at run time into a private temporary directory, which the cleanup removes:

- an HTML bundle in three versions: `index.html`, `page2.html`, `css/site.css`, `img/chart.png` (and
  `notes/v3.txt` in v3 only). `index.html` carries a harmless probe. It only tries to reach the parent page, the
  cookies, storage, the hub API, the top page and a popup, and writes the outcome labels into its own `#probe`
  element;
- a markdown bundle (`index.md`, `img/dot.png`, `notes.md`) with raw HTML and script that must stay inert.

The PNGs are built from bytes in code; nothing binary is committed.

## Grading

As in the access journey: the UI observation decides each row, and the API statuses are recorded and must agree.
Row assertions are soft, a row whose precondition failed is `not run`, and a "hidden" check follows a positive
anchor.

- The isolation rows (R2-05, R2-06, R2-13, R2b-06) record named checks and assert nothing themselves. Each row is then decided
  by one generic assertion ("R2 isolation row failed; see restricted evidence") from a single call site, so the output
  does not tell which check failed, or how many. The checks and their inputs go in the restricted evidence only
  (`rows[...].ui.checks`, `probes`, `frameRequests`, `frameSessionAll`, `frameSessionAlert`). The HTML reporter would also
  render each inner browser action and its error, so the render config uses only the list reporter (and github in
  CI).
- A frame request whose Cookie header could not be read counts as unknown, which fails the session check. It never
  counts as "no session". An Authorization header on a frame-started request fails it as well.
- The frame's own navigation to the hub API path is graded on what it carried: no Cookie and no Authorization header.
  Its HTTP status depends on how the hub is configured to sign users in, so it is recorded, never graded; a status that
  means something needs a hub configured as production is (no development sign-in). The Set-Cookie cookie names (names
  only) of responses to frame-started document requests, and the hub's start flags (names only, `hubStartFlags`), are
  recorded.
- The session checks count requests started by the frame's content. They exclude exactly one class: the iframe
  element's own document load started by the app page, recognised only when Sec-Fetch-Site is same-origin,
  Sec-Fetch-Dest iframe, Sec-Fetch-Mode navigate and the URL is exactly a frame src the app set for a mint. Its cookie
  presence is still recorded. Any missing or different header, or a URL mismatch, counts as frame-started (graded).
  R2-04 and R2-05 check that in-frame link navigations are classified as frame-started.
- Rows depend on facts, not on other rows passing: the bundle exists, the markdown was published, v3 exists, the
  owner's view URL was minted, the R2b thread, side artifact or direct message was set up. A failed R2-01 smoke stops every later row; a refused markdown publish makes the
  markdown rows `not run` with the cause.
- The journey refuses to run (before any identity exists) unless the resolved settings are the render config,
  reporters list, github or json only, and trace, video and screenshot off. So `--reporter=html`, `--trace`, `--ui`
  and `--debug` are refused for this suite: they would record each inner action of the isolation rows.
- Playwright's failure page snapshot (`error-context.md`) is turned off (`PLAYWRIGHT_NO_COPY_PROMPT`, set by the
  render config) for the same reason as traces: it could capture frame content. The journey refuses to run without it.
- In the cleanup (so also after a timeout), a whole-run sweep checks every frame request of every identity for a
  Cookie header. It decides no row; its result is restricted evidence (`frameSessionAll`). `swept` other than `true`
  means unknown, never clean, and raises the restricted `frameSessionAlert` like a session-bearing or unread request.
- R2-17b (view expiry) is declared and skipped with a `NOT RUN` annotation: the view lifetime is a fixed constant.
- R2-20x and R2b-11x are recorded only (status `observed`).
- Text polls are bounded exactly: each read's timeout is capped by the time left in the poll. Every poll is recorded
  (`polls`: step, what, bound, elapsed, reads, matched).

## The HTML bundle in the chat preview (R2b)

The R2b rows supersede the former R2-21 placeholder (an HTML bundle in the chat preview), which is removed. They run
in the same journey, after R2-20, so the R2 rows act as a regression check in every R2b run.

- Fixtures (API, not graded): a thread in the home project with one message that holds the bundle's reference twice,
  unpinned and pinned to v1 (`scion://artifact/<id>@1`). The sender side through the composer is not tested. A second
  project, which the member is not in, holds an artifact published through New artifact; a second message references
  it. The owner also sends the outsider a direct message with the bundle's reference.
- Each R2b row opens the thread on its legacy route (`/chat/space/<projectId>/thread/<topicId>`), which the chat page
  rewrites in place to `/chat/<slug>/<topicId>` once it knows the project's slug. The row waits (bounded) until the
  address bar shows the canonical route before it captures anything, and "the page stays on the thread" compares the
  thread (slug and topic id), not the raw URL. Both routes and the settle time are recorded (`chatHtml.routes`, ids
  only).
- The member opens the references through real clicks on the linkified text (R2b-01..R2b-09): the dialog, the
  frame's sandbox against the artifact page's (R2-13), the bundle's CSS, image and relative link, the frame's
  isolation (R2b-06, one generic verdict like R2-05), closing the dialog, the pinned version, and Open in artifact
  viewer. R2b-08a is a known-defect row (annotation `known-defect`): a pinned reference's Open in artifact viewer
  should open that version's page (ptone/scion#5602); it is expected to fail until that is fixed.
- R2b-10: the outsider cannot reach the thread. R2b-11: the member opens the reference to the artifact it cannot
  read and sees "Artifact unavailable", with no view minted and the same answer as for a random id. R2b-11x records
  the same for the outsider's direct message; a sender warning there depends on ptone/scion#4905.
- Cleanup (part of R2-18, oracle row R2b-12): the side artifact and project, the R2b thread and the direct message
  (soft-deleted; the direct-message pair has no delete route and goes with the throwaway hub), and the member's held
  view URL answering 404 after the delete.

## Evidence

After the cleanup the journey writes `web/test-results/artifacts-access/artifacts-render-evidence.json` (mode 0600).
There are no traces, videos, screenshots or HARs. Before writing, the journey checks the serialised evidence for
secret-shaped strings and view capabilities (R2-19). A view URL is a bearer capability: it is kept in memory only,
and every path in the evidence has it redacted. Cookies are recorded by name only, never by value.

`journey-core.ts` holds the shared journey helpers. For now only this journey uses them; moving the access journey
onto them is a follow-up.
