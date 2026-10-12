---
title: Browser Sessions as Test Identities
description: Using scion hub test-identity web-code and the e2e harness helpers to open a browser session as a test identity on a test hub.
---

This guide is for anyone who writes browser tests (for example with Playwright) against a test hub. It shows how a browser context can hold a hub web session as a **test identity**: a short-lived synthetic member or viewer user the hub issues for testing. No sign-in provider and no test-login endpoint are involved.

The flow has three steps:

1. Issue a test identity (`scion hub test-identity issue`).
2. Mint a single-use **web-login code** for it (`scion hub test-identity web-code`).
3. Redeem the code in the browser context (`POST /auth/test-identity/redeem`). The context then holds a session cookie for the identity.

The routes are described in the [API reference](/scion/reference/api/) under `/api/v1/test-identities`, and the commands in the [CLI reference](/scion/reference/cli/) under `scion hub test-identity`.

## Hub requirements

Browser sessions for test identities are available only on a test hub that has both of these:

- **Test identities enabled at the member level**: the hub starts with `--enable-test-identities`.
- **The test infrastructure marker** in its database, written with `scion server test-infra mark --hub-id <hub-id>` before the hub starts. Marking generates a hub-local key that only these sessions use. Unmarking or marking again replaces the key, which ends every test identity browser session.

There is no separate switch for web sessions. When either requirement is missing, minting a code answers `404` with `details.reason: test_identity_web_sessions_unavailable`, and the member tier keeps working without web sessions. Hubs behind proxy auth (for example IAP) also need at least one driver principal; see [IAP-fronted hubs](#iap-fronted-hubs).

`GET /api/v1/test-infra/status` reports `testIdentityWebSessions` (whether these sessions are available) and `testIdentityWebSessionsUnavailableReason` (why not, while test identities are on; empty otherwise). The admin health summary has the same two values.

## Minting a code

You need the `test_identity.issue` permission, as for issuing: an admin sign-in, or a hub-bound access token carrying the `test_identity:issue` scope in `SCION_HUB_TOKEN`.

```bash
dir=$(cd -P "$(mktemp -d)" && pwd)   # a private, task-owned directory, symlinks resolved
scion hub test-identity issue --role member --purpose "browser run" --out "$dir/identity.token" --json > "$dir/identity.json"
ID=$(jq -r .identity.id "$dir/identity.json")
scion hub test-identity web-code "$ID" --out "$dir/code.txt"
```

`web-code` writes **only** the code to `--out`. The path must not exist yet: an existing file, a symlink, or a path under a symlinked directory is refused before any request is sent. That includes system temp directories that sit under a symlink, such as `/var/folders/...` on macOS, which is why the example resolves the directory first. The file is created with mode `0600`. The code is never printed; the command prints the identity ID, the code ID, the code expiry and the file path (`--json` prints the same as JSON).

The code is valid for **5 minutes** and can be redeemed **once**. Minting a new code replaces any earlier code that was not redeemed.

## Redeeming the code in Playwright

The e2e harness in `web/e2e/harness/auth.ts` has two helpers:

- `redeemTestIdentity(context, { codeFile })` reads the code file, posts the code to `/auth/test-identity/redeem`, expects `204`, and removes the file. The session cookie is now in the context. Options: `code` instead of `codeFile`, `baseURL` (defaults to the context's), and `proxyAuthorization` (sent as `Proxy-Authorization`). On any other answer it throws with the status and the hub error code only, never a request or response body.
- `assertTestIdentity(pageOrRequest, { email, role })` checks who the session is (`GET /auth/me`, and `GET /api/v1/auth/me` for the same email) and returns `{ email, role, expiresAt }`.

```typescript
import { assertTestIdentity, redeemTestIdentity } from './harness/auth.js';

const context = await browser.newContext({ baseURL: hubURL });
await redeemTestIdentity(context, { codeFile }); // before any navigation
await assertTestIdentity(context.request, { email, role: 'member' });

const page = await context.newPage();
await page.goto('/', { waitUntil: 'domcontentloaded' });
// ... journeys; assert the identity again after each
await assertTestIdentity(page, { email });
```

While the session lasts, every page shows a "Test identity &lt;email&gt; · expires &lt;time&gt;" chip under the test-hub banner.

### Rules for test code

- **Redeem first**, in a fresh context, before any navigation.
- **Traces:** the helper pauses trace recording around the redeem, so a trace never holds the redeem request or its `Set-Cookie`. Actions taken in the same context before the redeem are dropped from the trace.
- A trace or HAR recorded after the redeem holds the session cookie, because every later request sends it. Treat such artifacts as sensitive until the identity is deleted or expires, and do not enable HAR recording (`recordHar`) on a context that redeems.
- Keep the code out of URLs, logs, screenshots and command lines: pass the code file, not the code.
- Do not write the context's `storageState` to a shared path.
- Use `waitUntil: 'domcontentloaded'`, not `networkidle`: the web app keeps server-sent event connections open.

## IAP-fronted hubs

Behind an authenticating proxy such as IAP, the browser reaches the hub as a **driver principal**, for example a test-UI service account, and IAP adds its verified assertion to every request. The hub treats a driver principal as a transport for test identity sessions, never as a user.

### Hub configuration

Two startup settings, under `server.auth.proxy` in `settings.yaml`, configure this. Neither can be written through the settings API, and the server-config view does not show them.

| Setting | Meaning |
| :--- | :--- |
| `driver_principals` | The exact emails of the driver principals. No wildcards or patterns. A hub whose `server.auth.mode` is not `proxy` refuses to start when this list is not empty. |
| `driver_token_audience` | The IAP OAuth client ID: the audience of the driver's Google ID token (see [The driver's bearer token](#the-drivers-bearer-token)). |

```yaml
server:
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: /projects/123456789/global/backendServices/987654321
      driver_principals:
        - test-ui-driver@my-test-project.iam.gserviceaccount.com
      driver_token_audience: 123456789-abc.apps.googleusercontent.com
```

A proxy hub with test identities on and **no** driver principals still starts. Web sessions are then unavailable: redeem answers `403`, any test identity session is ended with `401`, and the member tier for API clients keeps working. The hub logs `test-identity web sessions unavailable: proxy auth mode with no driverPrincipals` at startup, and the status endpoints report the reason `proxy_auth_without_driver_principals`.

### Driver principals have no hub account

A driver principal is treated like the hub's transport service account everywhere: it cannot sign in through any login flow, an admin cannot create or invite a user for it, and a session or hub token it held before it was listed stops working. A verified assertion for a driver principal never signs in or creates a user, so a run whose session has ended fails loudly instead of acting as the service account:

- page navigations, and `/api/v1/` requests that carry only the assertion, get `403` with `details.reason: driver_principal_has_no_session`;
- `/api/v1/` requests that carry the driver's ID token in `Authorization` (as the Playwright hook sends them) get `401`, or a plain `403` where the hub trusts Google tokens. They never act as the driver.

### Redeeming behind IAP

Redeem the code from Node (Playwright's `context.request`) with the driver's ID token in `Proxy-Authorization`, which IAP consumes. The hub checks IAP's assertion itself and requires a driver principal. It records the driver in the new session. A redeem without a valid assertion, or from a principal that is not a driver, is refused (`401` or `403`) before the code is read, so the code stays usable. Each refusal is audited with the reason, the first 8 hex digits of the code's hash (never the code), the client IP and the assertion email. Redeem is rate-limited per client IP and per assertion email. Behind the proxy the connection comes from the proxy itself: the hub reads the client IP from `X-Forwarded-For` only when the connecting peer is one of its trusted proxies, and otherwise records and limits the connecting address. There is no setting for the hub's trusted proxies yet, so behind IAP `client_ip` and the per-IP limit are the proxy's address: the per-IP limit is coarse there, and the per-email limit is the effective one.

On hubs without proxy auth nothing changes: redeem needs no assertion and ignores one.

### The session is bound to its driver

Behind the proxy, the hub honours a test identity session only on a request that carries a valid assertion for the driver that redeemed it. This applies to page routes, `/auth/me`, `/api/v1/` requests, the event stream (when it opens and on each 30-second heartbeat) and logout. With another principal's assertion, or none, the session is cleared, so a copied cookie is useless to anyone else behind the same IAP, and to a browser without IAP. The request itself gets `401`; on `/api/v1/` it goes on without the session and gets only what its assertion or bearer alone gets, never the test identity. A session made on a hub without proxy auth is never honoured behind the proxy, and the reverse.

An open event stream also ends when the assertion it was opened with expires. The browser then reconnects with a fresh one.

### The driver's bearer token

Pages cannot send `Proxy-Authorization`, so page requests carry the driver's Google ID token in `Authorization`. On `/api/v1/` such a header normally wins over the session cookie. The hub replaces it with the test identity's own short-lived token only when all of these hold:

- the hub is in proxy auth mode and `driver_token_audience` is set;
- the request carries a valid test identity session, bound to its driver as above;
- the bearer is not a hub-issued token (not a hub user token, personal access token, agent token or development token);
- the bearer verifies as a Google-signed ID token whose only audience is `driver_token_audience`;
- its email equals both the current assertion's email and the session's driver.

Otherwise the request is handled as it is today: a hub-issued bearer acts as itself, and any other bearer goes to the external-token path or gets `401`. The replacement can only narrow access: the request acts with exactly the test identity's permissions, never the driver's or more.

### Playwright

The harness's `installIapDriver(context, { baseURL, token })` (`web/e2e/harness/iap-driver.ts`) adds the driver's ID token to the context's requests to the hub origin (`baseURL`, required) only, never to another origin. It calls your `token` callback for a fresh token shortly before the cached one expires, and never logs or saves the token.

```typescript
import { redeemTestIdentity } from './harness/auth.js';
import { installIapDriver } from './harness/iap-driver.js';

const context = await browser.newContext({ baseURL: hubURL });
await redeemTestIdentity(context, { codeFile, proxyAuthorization: `Bearer ${await driverToken()}` });
await installIapDriver(context, { baseURL: hubURL, token: driverToken });
const page = await context.newPage();
await page.goto('/', { waitUntil: 'domcontentloaded' });
```

## Session lifetime and teardown

The session ends when the identity expires, when it is deleted, or when the hub's key changes. There is no refresh. After that, the next request with the session cookie answers `401` (an API or `fetch` request) or shows a "session ended" page (a browser navigation).

To tear down, close the context's pages, then delete the identity:

```bash
scion hub test-identity delete "$ID"
rm -rf "$dir"
```

## Reference use in this repository

The `test-identity-session` Playwright suite (`web/e2e/test-identity-session/`) runs this flow end to end in the Web E2E workflow. Its global setup starts a throwaway member-level test hub with the shared harness: `startHub({ testLogin: false, testIdentities: true })` gives the hub a random hub id, marks its temp database with `scion server test-infra mark`, and runs it with a private HOME. The spec issues an identity and mints a code with the CLI as the dev-auth admin, redeems the code, runs a UI journey, and checks that `/auth/me` answers `401` after the identity is deleted. The harness README (`web/e2e/harness/README.md`) has the details.
