# IAP browser journey (operator-run)

A Playwright journey against a live IAP-fronted test hub (ptone/scion#4240,
phase X5). It opens a browser session as a member test identity through a
scion target-hub profile, with the driver's IAP-audience ID token obtained
in-process from the runner's metadata emulator, and checks that no file holds
a driver token or a storage state.

**Operator-run only.** The spec skips with a clear reason unless
`IAP_JOURNEY=1` and its environment are set. No CI job or lane runs it, and
`.github/workflows/web-e2e.yml` deliberately has no matrix entry for it. Live
runs need uat-lead's go-ahead.

```bash
cd web
npm ci
npx playwright install chromium
IAP_JOURNEY=1 IAP_JOURNEY_PROFILE=t1 \
  npx playwright test -c e2e/iap-journey/playwright.config.ts
```

Preconditions, the token paths, the variables and the rules are in the
docs-site guide "IAP Driver Tokens for Browser Journeys"
(`docs-site/src/content/docs/contributing/test-identity-iap-driver.md`).
The configuration and token helpers are self-tested without a hub in
`web/e2e/harness-selftest/` (`npm run test:e2e-support`).
