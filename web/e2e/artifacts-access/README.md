# Artifact access journey (R1-A)

Playwright journey for artifact access, graded at the UI per role. It runs
on a hub with the `hub.artifacts` experiment (J3, plus the access part of
J2). See the header of `artifacts-access.pw.ts` for the rows.

## Run

Local throwaway hub (default). Builds the binary and web assets unless
`E2E_PREBUILT=1`:

```bash
cd web
npx playwright test -c e2e/artifacts-access/playwright.config.ts
```

R1-A needs no runtime broker, no container runtime and no credentials. The
hub starts with `hub.artifacts` at its default (off). The journey checks
the experiment-off rows first. It then enables the experiment through the
admin experiments API and resets the override during cleanup.

Five member test identities each act in their own browser context:

| Key | Role |
| --- | --- |
| `owner` | publishes the artifact through the web New artifact dialog; owns both fixture projects |
| `member` | `project-member` of the artifact's home project |
| `padmin` | `project-admin` of the home project |
| `grantee` | member of a second fixture project (`side`) with the owner, not of the home project; gets Can edit through the Share dialog and publishes v2 |
| `outsider` | in no project |

Existing non-IAP hub: set `E2E_TARGET_URL`. For each identity key, also set
`E2E_R1_<KEY>_CODE_FILE` and `E2E_R1_<KEY>_EMAIL` (OWNER, MEMBER, PADMIN,
GRANTEE, OUTSIDER). Each pair is a member test identity issued out of band
(`scion hub test-identity issue` / `web-code --out`). The redeem removes
each code file. With `E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1` and
`E2E_ADMIN_TOKEN_FILE`, the experiment is toggled as on the local hub.
Without them, the experiment-off rows are not run, and `hub.artifacts`
must already be on. The runner deletes the identities afterwards. The spec
cannot verify that, and it writes its evidence before then.

## Grading

Each row records its UI observation (controls visible and hidden, the 404
page, labels) and the API statuses (capability flags, probes). The UI
observation decides the row; a passing API check alone never passes it.
Row assertions are soft, so one failed row does not hide the others. A row
whose precondition failed is recorded as `not run` with the reason.

- Smoke (R1-01): if the owner's publish is refused (401/403), the run
  stops. It records a `FINDING` and marks every later row `not run`. There
  is no scope or role workaround.
- R1-06x is an observation and is not graded. It records whether the
  Share dialog's people search offers a user who shares no project with
  the owner.
- R1-B1..B5 need an agent-owned artifact, which means a broker and a
  publishing agent (the J4 fixture). They are skipped and marked `NOT RUN`
  with that reason.

## Evidence

No traces, videos, screenshots or HARs (they would hold session
cookies). After the cleanup, the spec writes
`artifacts-access-evidence.json` under `web/test-results/artifacts-access/`.
It holds no cookies, tokens or codes, and contains:

- `rows`: per R1-xx: role, status, reason, `ui` and `api` records, errors.
- `stateSequence`: every artifact-page snapshot in order (who, step,
  visible and hidden controls, 404, owner and version labels).
- `requests`: each identity's browser request log (method, path, status).
- `smoke`, `experiment`, `peopleSearch`, `findings`, `identities` (ids and
  roles), `projects`, `membershipFixtures`, `artifact`.
- `results`: per test title, with every Playwright annotation.
- `oracleRowMap`: R1-xx to the test that carries it.
- `cleanup`: the artifact delete, membership removals, project deletes,
  experiment reset and identity deletes, plus `final.states`, the direct
  re-reads. Any remaining resource is listed in `residue` and fails the
  afterAll.

Selectors live in `selectors.ts` only. The artifact pages have no
`data-testid` yet, so they are role, text and structure selectors.

`journey-lib.ts` holds local copies of helpers from the async create
journey, to be consolidated into `web/e2e/harness` once both are on main.
