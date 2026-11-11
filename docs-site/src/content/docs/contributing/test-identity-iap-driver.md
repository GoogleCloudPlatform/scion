---
title: IAP Driver Tokens for Browser Journeys
description: How a runner's Playwright driver obtains its IAP-audience ID token from the metadata emulator at run time, opens a test identity browser session through a target-hub profile, and runs the IAP browser journey.
---

This guide is for operators and test authors who run Playwright journeys from a Scion runner (an agent container) against an **IAP-fronted test hub** named by a [target-hub profile](/scion/hosted/user/test-identity-target-hubs/). It describes how the driver gets the Google ID token that IAP and the hub expect, how it opens a browser session as a test identity, and what it must never write to disk. The hub side (driver principals, the driver's bearer token, session binding) is described in [Browser Sessions as Test Identities](/scion/contributing/test-identity-browser-sessions/#iap-fronted-hubs).

## The rules in short

- The driver's ID token is obtained **in-process** from the runner's metadata emulator, each time it is needed. It is never written to a file, put in an environment variable, passed on a command line or logged.
- The web-login code goes only to a task-owned `0600` file written by `scion hub test-identity web-code --out`. The file is deleted right after the redeem.
- No cookie export and no Playwright `storageState` file: the session cookie lives only in the browser context's memory.
- Logs name only which token path was taken and HTTP status codes: no token, code or email.

## Preconditions

The runner's identity is determined at run setup; either token path below works. Before a run, check:

1. **Assign mode.** The runner's metadata emulator runs in assign mode (`SCION_METADATA_MODE=assign`) and `GCE_METADATA_HOST` is a loopback address. Confirmed by uat-lead (2026-10-16) on the uat-lead runner: `GET /computeMetadata/v1/instance/service-accounts/default/identity?audience=<aud>&format=full` answers `200` with a Google-signed ID token for the assigned service account (issuer `accounts.google.com`, the requested audience, an `email` claim). Other runners, such as a scratch VM lane, are checked at their run setup.
2. **The driver principal.** The target's IAP service account (the profile's `uatServiceAccount`, `<T>-uat`) is listed in the target hub's `auth.proxy.driverPrincipals`, and the target's `auth.proxy.driverTokenAudience` is the IAP OAuth client ID (the profile's `iapAudience`). Browser sessions are used for member and viewer test identities.
3. **Token creation for the driver.** When the runner's assigned service account is not the driver principal (the usual case), it holds `roles/iam.serviceAccountTokenCreator` (or `roles/iam.serviceAccountOpenIdTokenCreator`) on `<T>-uat`. The grant is made ahead of time by the hub's operators; a journey never grants anything and stops on a `403` from IAM Credentials.
4. **A target-hub profile** for the hub with `--transport iap`, its IAP audience, its IAP service account and a member issuer delegation (see [Test identities on a target hub](/scion/hosted/user/test-identity-target-hubs/)).

## Obtaining the driver token

Two paths, chosen from the emulator's answer to `GET /computeMetadata/v1/instance/service-accounts/default/email` (every emulator request carries `Metadata-Flavor: Google`):

| Assigned service account | Path | Requests |
| :--- | :--- | :--- |
| is the driver principal | **direct** | `GET .../service-accounts/default/identity?audience=<IAP client ID>&format=full` on the emulator returns the ID token. |
| is another account | **impersonate** | `GET .../service-accounts/default/token` on the emulator returns an access token for the assigned account; `POST https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/<driver>:generateIdToken` with `{"audience": "<IAP client ID>", "includeEmail": true}` returns the driver's ID token. The access token is sent only to IAM Credentials. |

The impersonate path is the one `scion --hub-profile` uses for its own IAP transport token. On the uat-lead runner the assigned account is not a `<T>-uat` account, so journeys there use the impersonate path.

The e2e harness implements both: `driverIdTokenSource({ driverServiceAccount, audience })` in `web/e2e/harness/driver-id-token.ts` returns a callback that fetches a fresh token on each call, refuses an emulator address that is not loopback or a metadata mode other than `assign`, and reports failures with the HTTP status only. Pass the callback to `installIapDriver()`, which caches the token in memory until shortly before it expires.

## Opening the session

```bash
export SCION_HUB_PROFILE=t1 SCION_HUB_RUN=r1
dir=$(cd -P "$(mktemp -d)" && pwd); chmod 700 "$dir"     # task-owned, symlinks resolved
scion hub test-identity issue --as journey --role member --purpose "browser journey" --json > "$dir/identity.json"
ID=$(jq -r .identity.id "$dir/identity.json")
scion hub test-identity web-code "$ID" --out "$dir/code.txt"   # the code goes only to this 0600 file
```

The issue output holds no token (the identity's token stays in the profile's run cache). Under a profile, `web-code` mints the code through the profile's issuer delegation, so it takes the identity ID and no `--as`.

```typescript
import { redeemTestIdentity } from './harness/auth.js';
import { driverIdTokenSource } from './harness/driver-id-token.js';
import { installIapDriver } from './harness/iap-driver.js';

const token = driverIdTokenSource({ driverServiceAccount: uatServiceAccount, audience: iapAudience });
const context = await browser.newContext({ baseURL: hubURL });            // no storageState
await redeemTestIdentity(context, { codeFile, proxyAuthorization: `Bearer ${await token()}` });
// redeemTestIdentity removed the code file
await installIapDriver(context, { baseURL: hubURL, token });
const page = await context.newPage();
await page.goto('/', { waitUntil: 'domcontentloaded' });
```

Then check who the session is: `GET /auth/me` and `GET /api/v1/auth/me` (from Node, with the driver token in `Proxy-Authorization`, and from the page) must show the test identity's `@scion-fixture.invalid` email, never the driver principal. Tear down with `scion hub test-identity teardown --run r1`; the next `/auth/me` answers `401`. Remove the task directory.

## The IAP browser journey

`web/e2e/iap-journey/` implements the steps above as one Playwright spec. It is **operator-run only**: it skips with a clear reason unless `IAP_JOURNEY=1` and its environment are set, and no CI job or lane runs it. It asserts that:

- the code file has mode `0600` and is gone after the redeem;
- `/auth/me` and `/api/v1/auth/me` show the fixture identity, never the driver, from Node and from the page;
- the session ends after teardown (`401`);
- no file under the task directory or the test output contains a driver token or a storage state.

Its config turns off traces, screenshots and video.

### Running it (operator step)

Live runs need uat-lead's go-ahead, and a lane window when a shared VM is used.

```bash
cd web
npm ci
npx playwright install chromium        # once per runner
IAP_JOURNEY=1 IAP_JOURNEY_PROFILE=t1 \
  npx playwright test -c e2e/iap-journey/playwright.config.ts
```

| Variable | Meaning |
| :--- | :--- |
| `IAP_JOURNEY=1` | The opt-in. |
| `IAP_JOURNEY_PROFILE` | The target-hub profile (transport `iap`); the hub URL, the driver principal and the IAP audience are read from it. |
| `IAP_JOURNEY_RUN` | Optional run label (default `iapj-<time>`). |
| `SCION_BIN` | Optional path of the `scion` binary (default `scion` on `PATH`). |
| `SCION_METADATA_MODE`, `GCE_METADATA_HOST` | Set by the runner; the emulator must be in assign mode. |

The output lists the token path taken and the HTTP status of each step. On a failure, run `scion hub test-identity teardown --run <label>` if the journey could not tear down.
