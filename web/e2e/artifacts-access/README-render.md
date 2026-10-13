# Artifact page view and render journey (R2)

A Playwright journey of the artifact page's view and render, graded at the UI. It lives in the same suite directory as
the access journey (`README.md`) and shares its global setup, state file and output dir. It has its own config,
`playwright.render.config.ts`, which has no HTML reporter (see Grading), and it skips itself under any other config. See
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

| Key        | Role                                                                         |
| ---------- | ---------------------------------------------------------------------------- |
| `owner`    | publishes both artifacts and all three bundle versions; owns the project     |
| `member`   | the reader: a member of the project, no grant                                |
| `outsider` | in no project                                                                |

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

- The isolation rows (R2-05, R2-06, R2-13) record named checks and assert nothing themselves. Each row is then decided
  by one generic assertion ("R2 isolation row failed; see restricted evidence") from a single call site, so the output
  does not tell which check failed, or how many. The checks and their inputs go in the restricted evidence only
  (`rows[...].ui.checks`, `probes`, `frameRequests`, `sessionBearingFrameRequest`). The HTML reporter would also
  render each inner browser action and its error, so the render config uses only the list reporter (and github in
  CI).
- A frame request whose Cookie header could not be read counts as unknown, which fails the session check. It never
  counts as "no session".
- Rows depend on facts, not on other rows passing: the bundle exists, the markdown was published, v3 exists. Only
  the R2-01 smoke and a refused markdown publish make rows `not run`.
- R2-17b (view expiry) and R2-21 (an HTML bundle in the chat preview) are declared and skipped with a `NOT RUN`
  annotation. The view lifetime is a fixed constant, and the chat HTML preview is not on this build.
- R2-20x is recorded only (status `observed`).

## Evidence

After the cleanup the journey writes `web/test-results/artifacts-access/artifacts-render-evidence.json` (mode 0600).
There are no traces, videos, screenshots or HARs. Before writing, the journey checks the serialised evidence for
secret-shaped strings and view capabilities (R2-19). A view URL is a bearer capability: it is kept in memory only,
and every path in the evidence has it redacted. Cookies are recorded by name only, never by value.

`journey-core.ts` holds the shared journey helpers. For now only this journey uses them; moving the access journey
onto them is a follow-up.
