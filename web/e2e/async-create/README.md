# Async agent create journey

Playwright journey for agent create from the web form on a hub with
`server.hub.async_agent_launch` on (ptone/scion#4664). See the header of
`async-create.pw.ts` for the checks.

## Run

Local throwaway hub (default). Builds the binary and web assets unless
`E2E_PREBUILT=1`:

```bash
cd web
npx playwright test -c e2e/async-create/playwright.config.ts
```

Where Docker works, also set `E2E_IMAGE_REGISTRY` (the registry the broker
pulls agent images from). The journey runs without credentials: agents
reach running through the shipped harness's no-auth fallback
(drop-to-shell), and the setup refuses to start while any credential
variable named in `harnesses/*/config.yaml` auth metadata is set.
`E2E_HARNESS_CONFIG` picks the harness config (default `antigravity`, the
form's default); its image is `<registry>/scion-<harness>:latest`, and the
setup log prints it. Without a usable container runtime the
broker still registers; the checks that need a real launch (A2/A3, A4)
are skipped with the reason, and the no-runtime failure panel check runs
instead. `E2E_LAUNCH_CAPABLE=1|0` overrides the detection.
`E2E_LAUNCH_TIMEOUT_MS` (default 300000) bounds each launch.

The bad image ref is a tag that does not exist in the same harness's
image repo (`E2E_BAD_IMAGE_REPO`, default `scion-<harness>`), so the
launch fails on the image pull; `E2E_BAD_IMAGE` gives the full ref
instead. The failure checks type it into the form's Container image
field; no template sets an image (templates do not carry one).

Existing non-IAP hub: set `E2E_TARGET_URL`, `E2E_TEST_IDENTITY_CODE_FILE`
and `E2E_TEST_IDENTITY_EMAIL` for a member test identity issued out of
band (`scion hub test-identity issue` / `web-code --out`), plus
`E2E_LAUNCH_CAPABLE=1` and `E2E_BAD_IMAGE` (an image ref whose tag does
not exist; the failure checks type it into the form's Container image
field and A4 asserts it exactly). Optional: `E2E_TARGET_PROJECT_ID`. The redeem removes the code file
(the code is single-use). The runner deletes that identity afterwards; the spec cannot
verify that, and its evidence is written before it. The no-runtime failure
panel check runs on a remote target only with `E2E_LAUNCH_CAPABLE=0`.
A remote hub must start the member's agents without harness credentials
(the no-auth fallback): A2 and A4 assert `appliedConfig.noAuth`, and fail
otherwise. The credential guard runs only for the local hub.

## Evidence

No traces, videos or HARs (they would hold the session cookie). After the
cleanup, the spec writes `async-create-evidence.json` (DOM timeline, SSE
launch payloads, request log, cleanup result; no cookies or tokens) under
`web/test-results/async-create/`.

Cleanup evidence: `cleanup.agents` gives each agent's DELETE answers,
`brokerDeleteFailed` (any 5xx, forced or not), whether it was gone before
the project delete, and `cleanAgentDelete`; `cleanup.final` re-reads every
agent, the project and the identity after all deletes. A
row removed only by the project delete is gone in `final` but not a clean
agent delete. `forcedDeletes` lists every force fallback next to
`launchCapable`. The panel checks record the page's own DELETE answers as
`panelDeleteStatuses`. In any status list, `null` means no answer (failed
or still pending), and `unansweredDeletes` counts those. `deleteForced`
means a force was attempted: it is set before the Force Delete click, so
it holds even if the force then fails. `a1.windowEnd` is when the A1 page
closed.

`E2E_KEEP_HUB_STATE=1` keeps the local hub's temp dir (database, private
HOME) at teardown and prints its path. It contains hub credentials: do not
copy it into evidence or artifacts.

Request records: `failed` means no response (`failure` gives the browser's
reason); `abortedAfterResponse` means answered and then aborted, which is
normal for the SSE stream when its page closes. `cleanup.agents[*]` and
`oracleRowMap` (row -> `{test, note}`) replace the earlier flat
`cleanup["agent X"]` keys and string map values.

A2 records the agent's `appliedConfig.image` but does not require it: on
an async create the hub does not yet record the image the broker
resolved (product defect D1, ptone/scion#4818 (part of #2153)), so
`a2AppliedConfig.a2AppliedConfigImageMissing` is true and the test carries
a D1 annotation.

A4 types the bad image ref into the form's Container image field
(Additional Options; request `config.image`, the broker's top image tier).
It first asserts and records the graded rows (failed-create panel, code
and message, Delete and Create again, no Start or Resume, no reload), then
deletes the agent from the panel, and only then pins the failure cause:
the failed agent's `appliedConfig.image` is the exact typed ref, with an
image failure code and the same credential-free auth and harness config
as A2; an empty or different image fails. A launch failure that names no
image cause sets `a4ImageReasonExposed: false` and a `FINDING` annotation
(recorded, not failed). Every Playwright annotation is mirrored into
`results[<test title>].annotations` in the evidence.

Selectors and state names live in `selectors.ts` only.
