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
pulls agent images from), and `E2E_HARNESS_CONFIG` if the form's default
harness cannot authenticate there. Without a usable container runtime the
broker still registers; the checks that need a real launch (A2/A3, A4)
are skipped with the reason, and the no-runtime failure panel check runs
instead. `E2E_LAUNCH_CAPABLE=1|0` overrides the detection.
`E2E_LAUNCH_TIMEOUT_MS` (default 300000) bounds each launch.

The bad image fixture template points at a tag that does not exist in
the agent image repo of that registry (`E2E_BAD_IMAGE_REPO`, default
`scion-claude`), so the launch fails on the image pull. `E2E_BAD_IMAGE`
gives the full image reference instead.

Existing non-IAP hub: set `E2E_TARGET_URL`, `E2E_TEST_IDENTITY_CODE_FILE`
and `E2E_TEST_IDENTITY_EMAIL` for a member test identity issued out of
band (`scion hub test-identity issue` / `web-code --out`), plus
`E2E_LAUNCH_CAPABLE=1`, `E2E_BAD_IMAGE_TEMPLATE` and optionally
`E2E_TARGET_PROJECT_ID`. The runner deletes that identity afterwards.

## Evidence

No traces, videos or HARs (they would hold the session cookie). After the
cleanup, the spec writes `async-create-evidence.json` (DOM timeline, SSE
launch payloads, request log, cleanup result; no cookies or tokens) under
`web/test-results/async-create/`.

Selectors and state names live in `selectors.ts` only.
