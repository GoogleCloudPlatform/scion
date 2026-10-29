# Agent-owned artifact journey (R1-B)

Playwright journey for access to an artifact owned by an agent, graded at
the UI per role. See the header of `artifacts-agent-owned.pw.ts` for the
rows and `rows.ts` for the row table and the oracle row map.

## Run

Local throwaway hub only (the spec's target-hub mode is not built;
`E2E_TARGET_URL` is refused). Builds the binary and web assets unless
`E2E_PREBUILT=1`:

```bash
cd web
E2E_IMAGE_REGISTRY=<registry> npx playwright test -c e2e/artifacts-agent-owned/playwright.config.ts
```

The global setup:

1. runs the credential guard (no harness credential variable may be set:
   the agent reaches running through the shipped harness's no-auth
   drop-to-shell fallback);
2. starts a member-level test hub (class test, `--test-mode member`,
   marked database, pinned hub id, private HOME) with the co-located
   runtime broker and the `hub.async_agent_launch` experiment on (set
   through the admin experiments API and read back);
3. with the hub's own issuer credential, turns `hub.artifacts` on and
   issues five member test identities and their single-use web-login
   codes (the recorded deviation: a local issuer, no workload delegation);
4. restarts the hub without development sign-in and reads that back: the
   old dev token must be refused.

The spec redeems the codes (they live 5 minutes) in its `beforeAll`.

| Key        | Role in the home project                                |
| ---------- | ------------------------------------------------------- |
| `powner`   | creates the project (project owner)                     |
| `aowner`   | project member; creates the agent (the delegating user) |
| `padmin`   | project admin                                           |
| `member`   | project member; later gets review access                |
| `outsider` | none                                                    |

aowner creates the agent from the web form, then types into its shell
through the keys operation and reads the terminal capture (the route
`scion look` uses): `scion whoami --full` must show both
`project:artifact:read` and `project:artifact:write` before
`scion artifact publish` runs (R1B-P8; otherwise the run is INVALID).

Environment:

- `E2E_IMAGE_REGISTRY`: the registry the broker pulls agent images from.
- `E2E_HARNESS_CONFIG`: harness config picked in the form (default
  `gemini-cli`: its no-auth fallback is a plain `exec bash`, while the
  other shipped harnesses first run an interactive login that would take
  the typed keys). `E2E_AGENT_IMAGE` pins the agent image (for example by
  digest): it is typed into the form's Container image field, and R1B-P7
  requires `appliedConfig.image` to equal it.
- `E2E_LAUNCH_CAPABLE=1|0`: whether agents can start here. Without it, a
  working `docker info` and `E2E_IMAGE_REGISTRY` decide. Not launch
  capable: every agent row is a declared skip
  (`NOT RUN: no container runtime in this lane`), which is the b02 gate
  tier; the rest still runs.
- `E2E_LAUNCH_TIMEOUT_MS` (default 300000): the agent launch.
- `E2E_OWNER_CHECK_TIMEOUT_MS` (default 900000): the wait for the
  owner-deleted mark, polled every 15 s. The mark comes only from the
  hub's 10-minute maintenance pass; nothing triggers it.

## Grading

- R1B-P1..P8 and R1B-01 are STOP rows: a failure records a `FINDING` and
  every later row is `not run` with that cause. No workaround of any kind.
- The UI observation decides each row, and the UI and the API must agree.
  An absent (omitempty) capability flag counts as false.
- R1B-08b is graded on the API (`403 review_subject_cannot_read`); the
  dialog is recorded.
- R1B-12, and aowner's leg of R1B-16, are observations while the product
  question on the delegating user after the agent's delete is open.
- R1B-06 and R1B-15 compare the outsider's answers with a missing id
  (status and code); their failures carry generic text, with the detail
  in `rows[id].ui`.
- Every row carries `deviation: "member test-mode; local issuer, no
workload delegation"`.

## Evidence

No traces, videos, screenshots or HARs. After the cleanup the spec writes
`web/test-results/artifacts-agent-owned/artifacts-agent-owned-evidence.json`
(checked for secret-shaped strings first, R1B-18): `rows`,
`stateSequence` (every artifact page snapshot), `agentShell` (each keys
call as sent and each capture, withheld if secret-shaped), `ownerCheckWait`
(each poll's time, status and body), `requests` per identity, `agent`,
`agentDelete`, `findings`, `invalid`, `cleanup` (with `final.states` and
`residue`), `identityChecksAfterRows`, `results` and `oracleRowMap`.

The identities and the experiment override end with the throwaway hub at
global teardown (no issuer credential exists after the restart); the
evidence says so in `cleanup.identities`.
