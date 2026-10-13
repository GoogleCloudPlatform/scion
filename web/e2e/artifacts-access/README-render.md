# Artifact page view and render journey (R2)

A Playwright journey of the artifact page's view and render, graded at the UI. It runs in the same suite as the access
journey (`README.md`) and shares its config, global setup and state file. See the header of `artifacts-render.pw.ts`
for the rows.

## Run

Local throwaway hub (default). Builds the binary and web assets unless `E2E_PREBUILT=1`:

```bash
cd web
npx playwright test -c e2e/artifacts-access/playwright.config.ts artifacts-render.pw.ts
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

- The isolation rows (R2-05, R2-06, R2-13) report every failure with one generic message only. Their detail goes in
  the structured evidence fields (`probes`, `frameRequests`, `sessionBearingFrameRequest`).
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
