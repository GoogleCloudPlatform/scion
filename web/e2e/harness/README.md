# E2E Test Harness

End-to-end tests run against the **real Go hub binary** with an ephemeral
SQLite database. No mocked APIs are used anywhere in the E2E path.

## Quick start

```bash
cd web
npm run test:e2e
```

This single command builds assets, starts the hub, seeds data, and runs all
Playwright specs. Artifacts land in `test-results/` and `playwright-report/`
(both gitignored).

## Architecture

```
global-setup.ts          ← Playwright calls this before any spec
  ├─ hub.ts              ← Builds web + Go binary, starts hub on ephemeral port
  ├─ auth.ts             ← Creates authenticated sessions via test-login,
  │                         and redeems test identity web-login codes
  └─ seed.ts             ← Seeds groups, users, role bindings via the real API
smoke.spec.ts            ← Proves the loop: login → navigate → assert → axe
global-teardown.ts       ← Kills the hub, cleans up temp files
```

## How the hub is started

1. `npm run build` produces the web assets in `web/dist/client/`.
2. `make build` compiles the Go binary to `build/scion`.
3. The binary is started with:
   - `--foreground` (no daemonization)
   - `--host 127.0.0.1` (loopback only — required for dev-auth)
   - `--web-port <ephemeral>` (random free port)
   - `--dev-auth` (the admin session comes from a dev sign-in link, `devSignInURL`)
   - `--enable-test-login` (programmatic session creation endpoint)
   - `--session-secret e2e-test-secret` (deterministic signing key)
   - `--db sqlite://<tmpdir>/e2e.db` (ephemeral database)
   - `--enable-hub --enable-web` (combined mode — API on the web port)
4. Setup waits for `GET /healthz` to return `{"status": "healthy"}`.

## Authentication strategy

### Admin user (dev-auth)

In workstation mode with `--dev-auth`, the first page load auto-creates a
session cookie for the development admin user (UUID
`be67fbc9-c869-5d43-b15d-c28ca3e8d355`, role `admin`).

### Programmatic multi-user auth (test-login)

For tests that need non-admin identities or multiple concurrent users:

1. The hub is started with `--enable-test-login` and a known
   `--session-secret`.
2. The test harness **derives the user-signing key** from the session secret
   using the same algorithm as the Go hub:
   ```
   sha256("scion-hub-signing-key:user_signing_key:" + sessionSecret)
   ```
3. It mints a short-lived HS256 JWT with audience `scion-test-login` (the
   "challenge token") using that derived key.
4. It calls `POST /api/v1/auth/test-login` with:
   - `Authorization: Bearer <challenge-token>`
   - Body: `{ "email": "...", "role": "admin|member|viewer", "displayName": "..." }`
5. The endpoint creates or updates the user, sets the session cookie, and
   returns an access token.
6. The session cookie is saved as a Playwright `storageState` JSON file.

### Using auth in specs

```typescript
import { getE2EEnv } from './harness/env.js';

test.describe('My feature', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState });

  test('admin can do X', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    // ...
  });
});
```

For a custom user, create a session in a `test.beforeAll`:

```typescript
import { createSession } from './harness/auth.js';
import { getE2EEnv } from './harness/env.js';

let viewerStorageState: string;

test.beforeAll(async () => {
  const env = getE2EEnv();
  const session = await createSession(env.baseURL, {
    email: 'viewer@e2e.test',
    role: 'viewer',
    displayName: 'E2E Viewer',
  });
  viewerStorageState = session.storageStatePath;
});

test.describe('Viewer restrictions', () => {
  test.use({ storageState: () => viewerStorageState });
  // ...
});
```

## Test identity browser sessions

A *test identity* is a short-lived synthetic member or viewer user that a
test hub issues (ptone/scion#4240). A browser context can hold a hub web
session as a test identity without test-login: the runner issues the
identity and mints a single-use **web-login code** with the CLI, and the
harness redeems the code in the browser context.

The hub must run with test identities enabled at the member level and
carry the test infrastructure marker (`scion server test-infra mark`),
which holds the hub-local key for these sessions. `startHub()` does both
with `testIdentities: true`:

```typescript
import { startHub, runScionCLI } from './harness/hub.js';

const state = await startHub({ testLogin: false, testIdentities: true, stateFile });
```

That throwaway hub gets a random hub id, a temp database marked before
start, and a private HOME and `SCION_*`-free environment under its temp
dir. `runScionCLI(state, args)` runs the CLI against it as the dev-auth
admin (the issuer). The `test-identity-session` suite
(`e2e/test-identity-session/`, run in the Web E2E workflow) is the
reference use.

### Hub settings and a co-located broker

Four more `startHub()` options, all off by default:

- `env`: extra environment of the hub process, for supported
  configuration such as `SCION_IMAGE_REGISTRY` or
  `SCION_SERVER_HUB_LAUNCHTIMEOUT`. Experiments are not environment
  settings; use `experiments` or `startupExperiments`. It is applied over the member-level hub's
  `SCION_*`-free environment. Keys the harness sets itself
  (`HARNESS_OWNED_ENV`: `HOME`, `SCION_CLI_MODE`, the hub id and hub class,
  the broker host) and the test mode and test identity settings
  (`SCION_SERVER_HUB_TEST*`, `SCION_SERVER_TEST*`) are refused: `startHub()`
  throws before it starts anything.
- `runtimeBroker: true` (needs `testIdentities: true`): runs the co-located
  runtime broker in the hub process on a free loopback port with
  `--auto-provide`, so a hub-managed project created on the hub gets it as
  its provider. The broker registers whether or not a container runtime
  works on the host; agents only start where one does. The broker needs an
  image registry setting (`SCION_IMAGE_REGISTRY` in `env`).
- `startupExperiments`: names of experiments the hub reads only at process
  start (for example `hub.conduit`, whose relay starts only if it is on
  then). `startHub()` turns each one on through the admin experiments API
  and restarts the hub on the same database, port, HOME and hub id. If
  anything fails before the restarted hub is healthy, the hub process is
  killed and `startHub()` throws.
- `experiments`: names of experiments the suite needs on. Once the hub is
  healthy (after any `startupExperiments` restart), `startHub()` turns
  each one on through the admin experiments API, with no restart, and
  reads the listing back. If one is not enabled, or a request fails, the
  hub process is killed and `startHub()` throws, so a suite never runs
  with an experiment silently off. The override is stored in the hub's
  database, so it survives `restartHubWithoutDevAuth`. The async create
  journeys use it for `hub.async_agent_launch`.

`restartHubWithoutDevAuth(stateFile)` restarts a hub that `startHub()`
started in the same process on the same database, port, HOME and hub id,
with the same arguments except `--dev-auth`, so a request without
credentials is not signed in, as on a hub in production. Do the setup that
needs the dev token first (test identities and their web-login codes,
memberships, fixtures): the dev token is not accepted afterwards, and the
state file is rewritten with the new PID, an empty `devToken` and
`devAuth: false`. Test identity web sessions, web-login codes and agent
and broker credentials keep working across the restart.

`E2E_KEEP_HUB_STATE=1` makes `stopHub()` keep the hub's temp dir (the
SQLite database, the private HOME and the hub's files) and print its
path, for inspecting a run afterwards. It is off by default. The dir
contains hub credentials (dev token, session keys, test identity rows):
do not copy it into evidence or artifacts, and remove it by hand when
done.

The `harness-selftest` suite (`e2e/harness-selftest/`) checks these
options against a stand-in binary, without Go or a browser:
`npm run test:e2e-support` (also run by the ci.yml web job).

```typescript
const state = await startHub({
  testLogin: false,
  testIdentities: true,
  runtimeBroker: true,
  env: { SCION_IMAGE_REGISTRY: registry },
  experiments: ['hub.async_agent_launch'],
  stateFile,
});
```

### Flow

```typescript
import { assertTestIdentity, redeemTestIdentity } from './harness/auth.js';

// 1. Issue (out of the browser). The token file is not needed for the
//    browser path; delete it.
const issued = JSON.parse(
  runScionCLI(state, ['hub', 'test-identity', 'issue', '--out', tokenFile, '--json'])
);
// 2. Mint a code into a new 0600 file in a task-owned temp dir, resolved with
//    fs.realpathSync (no symlinked directory in the path).
runScionCLI(state, ['hub', 'test-identity', 'web-code', issued.identity.id, '--out', codeFile]);

// 3. Redeem in a fresh context, before any navigation.
const context = await browser.newContext({ baseURL: state.baseURL });
await redeemTestIdentity(context, { codeFile }); // reads, redeems, removes codeFile
await assertTestIdentity(context.request, { email: issued.identity.email, role: 'member' });

// 4. Journeys; assert the identity again after each.
const page = await context.newPage();
await page.goto('/', { waitUntil: 'domcontentloaded' });
// ...
await assertTestIdentity(page, { email: issued.identity.email });

// 5. Teardown: close pages, delete the identity; /auth/me then answers 401.
```

### Helpers

| Function | What it does |
| --- | --- |
| `redeemTestIdentity(context, opts)` | `POST /auth/test-identity/redeem` with `{code}` from `opts.code` or `opts.codeFile`; expects 204 and leaves the session cookie in the context. With `codeFile`, the file is read and removed after a successful redeem. `opts.baseURL` defaults to the context's baseURL. `opts.proxyAuthorization` is sent as `Proxy-Authorization` (IAP-fronted hubs). Any other answer throws with the status and the hub error code only, never a body. |
| `assertTestIdentity(target, {email, role?})` | `target` is a page or a request context. Reads `GET /auth/me` and fails on an email mismatch, on a role mismatch when `role` is given, or when the session is not a test identity session; also checks that `GET /api/v1/auth/me` answers for the same email. Returns `{email, role, expiresAt}`. |
| `installIapDriver(context, {baseURL, token})` (`iap-driver.ts`) | IAP-fronted hubs only. Adds `Authorization: Bearer <token>` to the context's requests to the hub origin (page navigations, `fetch`, XHR, event streams), never to any other origin, and keeps an `Authorization` header a page sets itself. `token` is an async callback that returns the driver's Google OIDC ID token (audience: the IAP OAuth client ID); it is called on first use and again 60 s before the cached token's `exp` (`refreshBeforeMs`), and concurrent requests share one call. The token is never logged and never written to disk, `storageState` or traces by the hook. If the callback fails, the request is aborted rather than sent without the token. `opts.baseURL` (required) is the hub URL; only its origin gets the header. Returns a function that removes the hook. Redeem still goes through `redeemTestIdentity` with `proxyAuthorization`. |
| `withTracingPaused(context, fn)` | Runs `fn` with trace recording paused (used by the redeem). |

### Rules

- The code is valid for **5 minutes** and can be redeemed **once**. A new
  code replaces an unredeemed earlier one.
- **Traces:** the redeem pauses trace recording (it ends the current trace
  chunk without saving it and starts a new one), so no trace holds the
  redeem request or its `Set-Cookie`. Actions in the same context before
  the redeem are dropped from the trace; redeem first.
- A trace or HAR recorded **after** the redeem holds the session cookie
  (every later request sends it). Treat such artifacts as sensitive until
  the identity is deleted or expires, and do not enable `recordHar` on a
  context that redeems (HAR recording is not paused).
- `--out` of `web-code` must not have a symlinked directory anywhere in its
  path, including system temp dirs on macOS (`/var` is a symlink there).
  Resolve a temp dir first: `fs.realpathSync(fs.mkdtempSync(...))`.
- Keep the code out of URLs, logs, screenshots and argv: pass `codeFile`,
  not the code. The CLI never prints the code.
- Write no `storageState` of a test identity session to a shared path.
- Use `domcontentloaded` (SSE keeps connections open).
- After teardown, the first request with the ended session answers 401
  and clears it; with `--dev-auth`, a later request without a session is
  logged in as the dev admin again. Close the pages before deleting the
  identity, and check `/auth/me` once.
- test-login (`createSession()`) is unchanged for the main suite.

### IAP-fronted hubs

On a hub behind IAP, every browser request must carry the driver's Google
OIDC ID token, and pages cannot send `Proxy-Authorization`. The hub must
list the driver principal in `auth.proxy.driverPrincipals` and set
`auth.proxy.driverTokenAudience` to the IAP OAuth client ID (see the
docs-site how-to "Browser Sessions as Test Identities").

```ts
import { redeemTestIdentity } from './auth.js';
import { installIapDriver } from './iap-driver.js';

const context = await browser.newContext({ baseURL: hubURL });
const driverToken = () => mintDriverIdToken(); // your callback; returns a fresh ID token
await redeemTestIdentity(context, { codeFile, proxyAuthorization: `Bearer ${await driverToken()}` });
await installIapDriver(context, { baseURL: hubURL, token: driverToken });
const page = await context.newPage();
await page.goto('/', { waitUntil: 'domcontentloaded' });
```

- Redeem from Node with `proxyAuthorization`, then install the hook before
  the first navigation.
- The session works only with the driver that redeemed it: a request with
  another principal's assertion, or none, clears the session.
- When the session has ended, the driver never acts as itself: page
  navigations get `403` with `driver_principal_has_no_session`, and API
  requests carrying the driver's ID token get `401` (or a plain `403`
  where the hub trusts Google tokens).
- `web/e2e/iap-driver/` checks the hook in a real browser against two local
  servers (no IAP needed).
- On a Scion runner, `driverIdTokenSource()` in `driver-id-token.ts` is the
  token callback: it gets the driver's ID token in-process from the metadata
  emulator (directly, or through IAM Credentials when the runner's account
  is not the driver), never from a file. `web/e2e/iap-journey/` is the
  operator-run journey that uses it (see the docs-site guide "IAP Driver
  Tokens for Browser Journeys").

## Seeding test data

Seed helpers in `seed.ts` call the real hub API using the admin access token.
Available helpers:

| Function                        | What it creates                                    |
| ------------------------------- | -------------------------------------------------- |
| `createGroup()`                 | An explicit group with name, slug, labels           |
| `addGroupMember()`              | A user or group member with a governance role       |
| `createRoleBinding()`           | A role binding for a principal at a scope           |
| `findRoleDefinition()`          | Looks up a role definition by name                  |
| `setMaxMembersPerGroupQuota()`  | Sets the `max_members_per_group` limit to a value   |
| `createAccessBoundary()`        | Creates an access constraint via the preview flow   |
| `seedSmokeData()`               | Seeds the minimum data for the smoke test           |

### Adding a new identity with specific permissions

1. Create the user session via `createSession()` (this creates the user in
   the hub's database).
2. Find the role definition: `findRoleDefinition(baseURL, token, 'hub-admin')`.
3. Create a role binding: `createRoleBinding(baseURL, token, { ... })`.
   `scopeType` defaults to `'system'`, whose `scopeId` defaults to `''`
   (the hub's representation of system scope; the `createRoleBinding`
   helper, not the hub, rejects a non-empty value). Any other scope, e.g.
   `'project'`, requires an explicit `scopeId`.
4. The user's next API call will reflect the new permissions.

## Navigation waits

**Always use `domcontentloaded`**, never `networkidle`. The hub uses
Server-Sent Events (SSE) for real-time updates, which keeps a connection
open indefinitely. `networkidle` would never resolve.

```typescript
// CORRECT
await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });

// WRONG — will time out
await page.goto('/admin/groups', { waitUntil: 'networkidle' });
```

## Flake policy

- No `waitForTimeout()` — use Playwright auto-waiting and `expect()` with
  built-in retries.
- Use semantic locators (`getByRole`, `getByText`, `getByLabel`) over CSS
  selectors.
- All assertions use `expect()` which auto-retries by default.

## Artifacts

| Path                | Contents                          | Gitignored |
| ------------------- | --------------------------------- | ---------- |
| `web/test-results/` | Screenshots, videos, traces       | Yes        |
| `web/playwright-report/` | HTML report                  | Yes        |

To view the report after a run:

```bash
npx playwright show-report
```
