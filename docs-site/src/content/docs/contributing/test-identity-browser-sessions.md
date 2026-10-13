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

- **Test identities enabled at the member level**: the hub starts with `--test-mode=member:<hub-id>` (or `admin:<hub-id>`), which itself requires `server.hub.class: test` and the marker below (see "Test mode (operators)" in the [API reference](/scion/reference/api/)).
- **The test infrastructure marker** in its database, written with `scion server test-infra mark --hub-id <hub-id>` before the hub starts. Marking generates a hub-local key that only these sessions use. Unmarking or marking again replaces the key, which ends every test identity browser session.

There is no separate switch for web sessions. When either requirement is missing, minting a code answers `404` with `details.reason: test_identity_web_sessions_unavailable`, and the member tier keeps working without web sessions. Hubs behind proxy auth (for example IAP) do not offer these sessions yet.

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

## Session lifetime and teardown

The session ends when the identity expires, when it is deleted, or when the hub's key changes. There is no refresh. After that, the next request with the session cookie answers `401` (an API or `fetch` request) or shows a "session ended" page (a browser navigation).

To tear down, close the context's pages, then delete the identity:

```bash
scion hub test-identity delete "$ID"
rm -rf "$dir"
```

## Reference use in this repository

The `test-identity-session` Playwright suite (`web/e2e/test-identity-session/`) runs this flow end to end in the Web E2E workflow. Its global setup starts a throwaway member-level test hub with the shared harness: `startHub({ testLogin: false, testIdentities: true })` gives the hub a random hub id, marks its temp database with `scion server test-infra mark`, and runs it with a private HOME. The spec issues an identity and mints a code with the CLI as the dev-auth admin, redeems the code, runs a UI journey, and checks that `/auth/me` answers `401` after the identity is deleted. The harness README (`web/e2e/harness/README.md`) has the details.
