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

The bad image fixture template points at a tag that does not exist in
the same harness's image repo (`E2E_BAD_IMAGE_REPO`, default
`scion-<harness>`), so the launch fails on the image pull. `E2E_BAD_IMAGE`
gives the full image reference instead.

Existing non-IAP hub: set `E2E_TARGET_URL`, `E2E_TEST_IDENTITY_CODE_FILE`
and `E2E_TEST_IDENTITY_EMAIL` for a member test identity issued out of
band (`scion hub test-identity issue` / `web-code --out`), plus
`E2E_LAUNCH_CAPABLE=1` and `E2E_BAD_IMAGE_TEMPLATE`. Optional:
`E2E_TARGET_PROJECT_ID`, and `E2E_BAD_IMAGE` (that template's image ref,
so A4 asserts it exactly; without it, A4 asserts the same harness config
as A2 and a different, non-empty image). The redeem removes the code file
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

Selectors and state names live in `selectors.ts` only.
