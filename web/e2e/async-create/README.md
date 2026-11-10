# Async agent create journey

Playwright journey for agent create from the web form on a hub with
the `hub.async_agent_launch` experiment on (ptone/scion#4664). The local
hub's global setup sets the experiment on through the admin experiments API
and reads it back before the journey runs. See the header of
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
form's default); its image is `<registry>/scion-<harness>:latest`, and
the setup log prints it. A2 requires `appliedConfig.image` to be exactly
the declared ref: `E2E_AGENT_IMAGE` when set, else that default. Without a usable container runtime the
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
field and A4 asserts it exactly), and `E2E_AGENT_IMAGE` (the image the harness config resolves to on that hub;
A2 asserts it exactly). Optional: `E2E_TARGET_PROJECT_ID`. The redeem removes the code file
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

A2 requires the agent's `appliedConfig.image` to be the expected image
and records it in `a2AppliedConfig` with the harness config, the
top-level `template`, and `profile` (null for an agent pinned to a broker,
which does not get one). On a local hub it also records the running
container's image (`docker inspect` `.Config.Image`) as
`a2AppliedConfig.container`, without checking it. `a2.launchSteps` (and
`a4.launchSteps`) record the launch steps seen: `sse`, the ordered steps
of the browser's SSE launch events with the time each first arrived, and
`ui`, the page's launch-step texts with their times and raw step names.
A2 requires the SSE steps to follow the canonical order (`LAUNCH_STEPS`
in `selectors.ts`) and each page text to be `Step: ` plus the label of
one of them (`src/shared/agent-launch-labels.ts`), in the same order; problems are listed in
`a2.launchStepProblems`. A fast launch may show no step in the page,
which is recorded only. `stateSequence` entries carry the step shown in
the banner.

A4 types the bad image ref into the form's Container image field
(Additional Options; request `config.image`, the broker's top image tier).
It first asserts and records the graded rows (failed-create panel,
launch-code and launch-error-message, Delete and Create again,
header-start, header-resume and header-force-resume all absent or not
visible, no reload), then deletes the agent from the panel, and only then
pins the failure cause: the failed agent's `appliedConfig.image` is the
exact typed ref, with the same credential-free auth and harness config as
A2; an empty or different image fails. The launch code is recorded only
(`a4.a3f`, with an `A3f RECORD ONLY` annotation): the code next to the
expected `runtime_error` (`A3F_EXPECTED_CODE`; a container runtime gives
no image-specific code), the end reason and its label, the message
length, the launch step, and the time from submit to the panel and to the
launch end, which tells a fast failure from a hanging pull
(`launch_timeout`). Each test's Playwright annotations are mirrored into
`results[<test title>].annotations` in the evidence; the A5 entry lists
only the annotations the afterAll added itself.

The agent page is read through its data-testids only. Selectors and state
names live in `selectors.ts`; the in-page recorder (`page-recorder.ts`)
takes them from there.
